package deletion

import (
	"context"
	"go.kenn.io/msgvault/internal/store"
)

func archiveTargetForTest(ctx context.Context, st *store.Store, id int64) (store.EmailTagTarget, error) {
	target, err := st.EmailTagTargetContext(ctx, id, "")
	if err != nil {
		return target, err
	}
	return st.CheckIMAPArchiveTargetContext(ctx, target)
}
