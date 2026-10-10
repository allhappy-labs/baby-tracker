package authority

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// authorityBegin opens a read-committed transaction and sets the
// transaction-local authority flag in the same simple-protocol round trip.
// app.authority_context() admits the transaction to authority rows and lets
// it write the guarded authority columns of accounts and families. If the
// set_config fails the connection is left in an aborted transaction, which
// the pool destroys on release.
const authorityBegin = `BEGIN ISOLATION LEVEL READ COMMITTED READ WRITE; SELECT set_config('app.authority','1',true)`

// begin opens an authority transaction on the runtime pool.
func (s *Service) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{BeginQuery: authorityBegin})
	if err != nil {
		return nil, fmt.Errorf("authority: begin: %w", err)
	}
	return tx, nil
}

// authorityBeginReadOnly opens a read-only repeatable-read authority
// transaction: one snapshot for every check of a command that writes nothing.
const authorityBeginReadOnly = `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY; SELECT set_config('app.authority','1',true)`

// beginReadOnly opens a read-only authority transaction on the runtime pool.
func (s *Service) beginReadOnly(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{BeginQuery: authorityBeginReadOnly})
	if err != nil {
		return nil, fmt.Errorf("authority: begin read-only: %w", err)
	}
	return tx, nil
}
