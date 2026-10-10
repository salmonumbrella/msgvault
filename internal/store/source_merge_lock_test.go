package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func waitForSourceOperation(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		require.FailNow(t, "source operation did not finish")
		return context.DeadlineExceeded
	}
}

func TestMergeSourcesExecutionLockOwnership(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse_%t", reverse), func(t *testing.T) {
			require := require.New(t)
			var st *Store
			if dbURL := os.Getenv("MSGVAULT_TEST_DB"); IsPostgresURL(dbURL) {
				st = newPGStoreInternal(t, dbURL)
			} else {
				st = openTestStore(t)
			}
			first, err := st.GetOrCreateSource("gmail", "first@example.test")
			require.NoError(err)
			second, err := st.GetOrCreateSource("gmail", "second@example.test")
			require.NoError(err)
			peer, err := OpenForTest(st.dbPath)
			require.NoError(err)
			t.Cleanup(func() { _ = peer.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			req := MergeSourcesRequest{FromSourceID: first.ID, IntoSourceID: second.ID, DryRun: true}
			if reverse {
				req.FromSourceID, req.IntoSourceID = req.IntoSourceID, req.FromSourceID
			}
			blocker, err := peer.AcquireSyncExecutionContext(ctx, second.ID)
			require.NoError(err)
			defer func() { _ = blocker.Release() }()
			_, err = st.MergeSourcesContext(ctx, req)
			require.ErrorIs(err, ErrSyncAlreadyActive)
			// Failure on the second lock must release the first physical lock
			// and its reservation in the merging store.
			for _, owner := range []*Store{peer, st} {
				execution, err := owner.AcquireSyncExecutionContext(ctx, first.ID)
				require.NoError(err)
				require.NoError(execution.Release())
			}
			require.NoError(blocker.Release())

			locked := make(chan struct{})
			resume := make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(resume) }) }
			defer release()
			st.sourceMergeBeforeIdentityLockHook = func() { close(locked); <-resume }
			done := make(chan error, 1)
			go func() { _, err := st.MergeSourcesContext(ctx, req); done <- err }()
			select {
			case <-locked:
			case <-ctx.Done():
				require.FailNow("merge did not acquire both execution locks")
			}
			for _, owner := range []*Store{peer, st} {
				for _, id := range []int64{first.ID, second.ID} {
					_, err := owner.AcquireSyncExecutionContext(ctx, id)
					require.ErrorIs(err, ErrSyncAlreadyActive)
				}
			}
			release()
			require.NoError(waitForSourceOperation(t, done))
			st.sourceMergeBeforeIdentityLockHook = nil
			for _, owner := range []*Store{peer, st} {
				for _, id := range []int64{first.ID, second.ID} {
					execution, err := owner.AcquireSyncExecutionContext(ctx, id)
					require.NoError(err)
					require.NoError(execution.Release())
					require.NoError(execution.Release())
				}
			}
		})
	}
}
