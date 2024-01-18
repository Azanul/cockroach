// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package batcheval

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/isolation"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/concurrency/lock"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/stretchr/testify/require"
)

// instrumentedEngine wraps a storage.Engine and allows for various methods in
// the interface to be instrumented for testing purposes.
type instrumentedEngine struct {
	storage.Engine

	onNewIterator func(storage.IterOptions)
	// ... can be extended ...
}

func (ie *instrumentedEngine) NewMVCCIterator(
	ctx context.Context, iterKind storage.MVCCIterKind, opts storage.IterOptions,
) (storage.MVCCIterator, error) {
	if ie.onNewIterator != nil {
		ie.onNewIterator(opts)
	}
	return ie.Engine.NewMVCCIterator(ctx, iterKind, opts)
}

// TestCollectIntentsUsesSameIterator tests that all uses of CollectIntents
// (currently only by READ_UNCOMMITTED Gets, Scans, and ReverseScans) use the
// same cached iterator (prefix or non-prefix) for their initial read and their
// provisional value collection for any intents they find.
func TestCollectIntentsUsesSameIterator(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	key := roachpb.Key("key")
	ts := hlc.Timestamp{WallTime: 123}
	header := kvpb.Header{
		Timestamp:       ts,
		ReadConsistency: kvpb.READ_UNCOMMITTED,
	}
	evalCtx := (&MockEvalCtx{ClusterSettings: cluster.MakeTestingClusterSettings()}).EvalContext()

	testCases := []struct {
		name              string
		run               func(*testing.T, storage.ReadWriter) (intents []roachpb.KeyValue, _ error)
		expPrefixIters    int
		expNonPrefixIters int
	}{
		{
			name: "get",
			run: func(t *testing.T, db storage.ReadWriter) ([]roachpb.KeyValue, error) {
				req := &kvpb.GetRequest{
					RequestHeader: kvpb.RequestHeader{Key: key},
				}
				var resp kvpb.GetResponse
				if _, err := Get(ctx, db, CommandArgs{Args: req, Header: header, EvalCtx: evalCtx}, &resp); err != nil {
					return nil, err
				}
				if resp.IntentValue == nil {
					return nil, nil
				}
				return []roachpb.KeyValue{{Key: key, Value: *resp.IntentValue}}, nil
			},
			expPrefixIters:    2,
			expNonPrefixIters: 0,
		},
		{
			name: "scan",
			run: func(t *testing.T, db storage.ReadWriter) ([]roachpb.KeyValue, error) {
				req := &kvpb.ScanRequest{
					RequestHeader: kvpb.RequestHeader{Key: key, EndKey: key.Next()},
				}
				var resp kvpb.ScanResponse
				if _, err := Scan(ctx, db, CommandArgs{Args: req, Header: header, EvalCtx: evalCtx}, &resp); err != nil {
					return nil, err
				}
				return resp.IntentRows, nil
			},
			expPrefixIters:    0,
			expNonPrefixIters: 2,
		},
		{
			name: "reverse scan",
			run: func(t *testing.T, db storage.ReadWriter) ([]roachpb.KeyValue, error) {
				req := &kvpb.ReverseScanRequest{
					RequestHeader: kvpb.RequestHeader{Key: key, EndKey: key.Next()},
				}
				var resp kvpb.ReverseScanResponse
				if _, err := ReverseScan(ctx, db, CommandArgs{Args: req, Header: header, EvalCtx: evalCtx}, &resp); err != nil {
					return nil, err
				}
				return resp.IntentRows, nil
			},
			expPrefixIters:    0,
			expNonPrefixIters: 2,
		},
	}
	for _, c := range testCases {
		t.Run(c.name, func(t *testing.T) {
			// Test with and without deletion intents. If a READ_UNCOMMITTED request
			// encounters an intent whose provisional value is a deletion tombstone,
			// the request should ignore the intent and should not return any
			// corresponding intent row.
			testutils.RunTrueAndFalse(t, "deletion intent", func(t *testing.T, delete bool) {
				db := &instrumentedEngine{Engine: storage.NewDefaultInMemForTesting()}
				defer db.Close()

				// Write an intent.
				val := roachpb.MakeValueFromBytes([]byte("val"))
				txn := roachpb.MakeTransaction("test", key, isolation.Serializable, roachpb.NormalUserPriority, ts, 0, 1, 0, false /* omitInRangefeeds */)
				var err error
				if delete {
					_, _, err = storage.MVCCDelete(ctx, db, key, ts, storage.MVCCWriteOptions{Txn: &txn})
				} else {
					_, err = storage.MVCCPut(ctx, db, key, ts, val, storage.MVCCWriteOptions{Txn: &txn})
				}
				require.NoError(t, err)

				// Instrument iterator creation, count prefix vs. non-prefix iters.
				var prefixIters, nonPrefixIters int
				db.onNewIterator = func(opts storage.IterOptions) {
					if opts.Prefix {
						prefixIters++
					} else {
						nonPrefixIters++
					}
				}

				intents, err := c.run(t, db)
				require.NoError(t, err)

				// Assert proper intent values.
				if delete {
					require.Len(t, intents, 0)
				} else {
					expIntentVal := val
					expIntentVal.Timestamp = ts
					expIntentKeyVal := roachpb.KeyValue{Key: key, Value: expIntentVal}
					require.Len(t, intents, 1)
					require.Equal(t, expIntentKeyVal, intents[0])
				}

				// Assert proper iterator use.
				require.Equal(t, c.expPrefixIters, prefixIters)
				require.Equal(t, c.expNonPrefixIters, nonPrefixIters)
				require.Equal(t, c.expNonPrefixIters, nonPrefixIters)
			})
		})
	}
}

func TestRequestBoundLockTableView(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	lockHolderTxnID := uuid.MakeV4()
	keyA := roachpb.Key("a")
	keyB := roachpb.Key("b")
	keyC := roachpb.Key("c")

	m := newMockTxnBoundLockTableView(lockHolderTxnID)
	m.addLock(keyA, lock.Shared)
	m.addLock(keyB, lock.Exclusive)

	// Non-locking request.
	ltView := newRequestBoundLockTableView(m, lock.None)
	locked, _, err := ltView.IsKeyLockedByConflictingTxn(keyA)
	require.NoError(t, err)
	require.False(t, locked)

	locked, _, err = ltView.IsKeyLockedByConflictingTxn(keyB)
	require.NoError(t, err)
	require.False(t, locked)

	locked, _, err = ltView.IsKeyLockedByConflictingTxn(keyC)
	require.NoError(t, err)
	require.False(t, locked)

	// Shared locking request.
	ltView = newRequestBoundLockTableView(m, lock.Shared)
	locked, _, err = ltView.IsKeyLockedByConflictingTxn(keyA)
	require.NoError(t, err)
	require.False(t, locked)

	locked, txn, err := ltView.IsKeyLockedByConflictingTxn(keyB)
	require.NoError(t, err)
	require.True(t, locked)
	require.Equal(t, txn.ID, lockHolderTxnID)

	locked, _, err = ltView.IsKeyLockedByConflictingTxn(keyC)
	require.NoError(t, err)
	require.False(t, locked)

	// Exclusive locking request.
	ltView = newRequestBoundLockTableView(m, lock.Exclusive)
	locked, txn, err = ltView.IsKeyLockedByConflictingTxn(keyA)
	require.NoError(t, err)
	require.True(t, locked)
	require.Equal(t, txn.ID, lockHolderTxnID)

	locked, txn, err = ltView.IsKeyLockedByConflictingTxn(keyB)
	require.NoError(t, err)
	require.True(t, locked)
	require.Equal(t, txn.ID, lockHolderTxnID)

	locked, _, err = ltView.IsKeyLockedByConflictingTxn(keyC)
	require.NoError(t, err)
	require.False(t, locked)
}

// mockTxnBoundLockTableView is a mocked version of the txnBoundLockTableView
// interface.
type mockTxnBoundLockTableView struct {
	locks           map[string]lock.Strength
	lockHolderTxnID uuid.UUID // txnID of all held locks
}

var _ txnBoundLockTableView = &mockTxnBoundLockTableView{}

// newMockTxnBoundLockTableView constructs and returns a
// mockTxnBoundLockTableView.
func newMockTxnBoundLockTableView(lockHolderTxnID uuid.UUID) *mockTxnBoundLockTableView {
	return &mockTxnBoundLockTableView{
		locks:           make(map[string]lock.Strength),
		lockHolderTxnID: lockHolderTxnID,
	}
}

// addLock adds a lock on the supplied key with the given lock strength. The
// lock is held by m.TxnID.
func (m mockTxnBoundLockTableView) addLock(key roachpb.Key, str lock.Strength) {
	m.locks[key.String()] = str
}

// IsKeyLockedByConflictingTxn implements the txnBoundLockTableView interface.
func (m mockTxnBoundLockTableView) IsKeyLockedByConflictingTxn(
	key roachpb.Key, str lock.Strength,
) (bool, *enginepb.TxnMeta, error) {
	lockStr, locked := m.locks[key.String()]
	if !locked {
		return false, nil, nil
	}
	var conflicts bool
	switch str {
	case lock.None:
		conflicts = false
		return false, nil, nil
	case lock.Shared:
		conflicts = lockStr == lock.Exclusive
	case lock.Exclusive:
		conflicts = true
	default:
		panic("unknown lock strength")
	}
	if conflicts {
		return true, &enginepb.TxnMeta{ID: m.lockHolderTxnID}, nil
	}
	return false, nil, nil
}