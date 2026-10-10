package maintenance

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgconn"
)

// AdaptiveBatch bounds one kind of prune within a pass. Each statement
// removes at most Size rows, so each transaction stays short however large
// the backlog (a request waiting on a row the prune holds waits for one
// small batch, not a large one). A batch that comes back full is followed at
// once by another, until a short batch or until the pass has removed
// PassCap rows. A statement timeout (the batch could not finish in 10s, as
// on slow storage) halves Size, down to Min, so a backlog still drains
// instead of rolling back on every pass; the smaller size persists.
//
// The zero size is Start. Drain may run beside itself; each call reads the
// current size per batch.
type AdaptiveBatch struct {
	Start, Min, PassCap int
	size                atomic.Int64 // 0: Start
}

// Size is the rows per statement the next batch uses.
func (b *AdaptiveBatch) Size() int {
	if size := b.size.Load(); size > 0 {
		return int(size)
	}
	return b.Start
}

// Drain runs prune with the current size while batches come back full (a
// batch removing at least the size it was given) and the pass is under
// PassCap. It returns the rows removed by the batches that committed,
// whether the pass stopped at its cap (a backlog the next pass continues),
// and the first failure, which ends the pass.
func (b *AdaptiveBatch) Drain(ctx context.Context, prune func(ctx context.Context, limit int) (int, error)) (removed int, capped bool, err error) {
	for {
		limit := b.Size()
		n, err := prune(ctx, limit)
		if err != nil {
			if statementTimedOut(ctx, err) {
				b.size.Store(int64(max(limit/2, b.Min)))
			}
			return removed, false, err
		}
		removed += n
		if n < limit {
			return removed, false, nil
		}
		if removed >= b.PassCap {
			return removed, true, nil
		}
		if err := ctx.Err(); err != nil {
			return removed, false, err
		}
	}
}

// statementTimedOut reports a statement the server cancelled at its
// statement_timeout: SQLSTATE 57014 while the caller's context is live (a
// cancelled context also ends in 57014, and says nothing about the batch).
func statementTimedOut(ctx context.Context, err error) bool {
	var pgErr *pgconn.PgError
	return ctx.Err() == nil && errors.As(err, &pgErr) && pgErr.Code == "57014"
}
