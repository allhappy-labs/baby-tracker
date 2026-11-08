//go:build identitytest

package loadseed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// copyFrozen writes source into table, which must be empty, with COPY
// FREEZE: in one transaction it locks the table, checks that it holds no
// row, truncates it (COPY FREEZE needs a table truncated in its own
// transaction) and copies. The rows are written frozen and their pages
// all-visible, so neither the readers that follow (index builds, key
// validation) nor the vacuum rewrite every page to set hint bits and the
// visibility map. Nothing may reference table by a foreign key while it
// loads (TRUNCATE refuses that). The check refuses a table with rows:
// copyFrozen never truncates data.
//
// pgx's CopyFrom has no options, so the binary COPY stream is built here
// as pgx builds it (copy_from.go: header, then each row's field count and
// each value encoded for its column's type).
func copyFrozen(ctx context.Context, pool *pgxpool.Pool, table string, columns []string, source pgx.CopyFromSource) (int64, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if err := registerTypes(ctx, conn.Conn(), "app.care_operation", "app.session_scope", "app.member_role", "app.session_status"); err != nil {
		return 0, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	ident := pgx.Identifier{table}.Sanitize()
	if _, err := tx.Exec(ctx, `LOCK TABLE `+ident+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		return 0, fmt.Errorf("loadseed: lock %s: %w", table, err)
	}
	var empty bool
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS (SELECT FROM `+ident+`)`).Scan(&empty); err != nil {
		return 0, fmt.Errorf("loadseed: %s: %w", table, err)
	}
	if !empty {
		return 0, fmt.Errorf("loadseed: refusing to load %s: it holds rows", table)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE `+ident); err != nil {
		return 0, fmt.Errorf("loadseed: truncate %s: %w", table, err)
	}
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = pgx.Identifier{c}.Sanitize()
	}
	list := strings.Join(quoted, ", ")
	description, err := conn.Conn().PgConn().Prepare(ctx, "", `SELECT `+list+` FROM `+ident, nil)
	if err != nil {
		return 0, fmt.Errorf("loadseed: describe %s: %w", table, err)
	}
	typeMap := conn.Conn().TypeMap()
	r, w := io.Pipe()
	written := make(chan struct{})
	go func() {
		defer close(written)
		buf := append(make([]byte, 0, 1<<16), "PGCOPY\n\377\r\n\000"...)
		buf = append(buf, 0, 0, 0, 0, 0, 0, 0, 0) // flags, header extension length
		for source.Next() {
			values, err := source.Values()
			if err == nil && len(values) != len(columns) {
				err = fmt.Errorf("loadseed: %d values for %d columns", len(values), len(columns))
			}
			if err != nil {
				w.CloseWithError(err)
				return
			}
			buf = append(buf, byte(len(columns)>>8), byte(len(columns)))
			for i, value := range values {
				if buf, err = encodeCopyValue(typeMap, buf, description.Fields[i].DataTypeOID, value); err != nil {
					w.CloseWithError(fmt.Errorf("loadseed: %s.%s: %w", table, columns[i], err))
					return
				}
			}
			if len(buf) > 1<<16-1<<12 {
				if _, err := w.Write(buf); err != nil {
					return
				}
				buf = buf[:0]
			}
		}
		if err := source.Err(); err != nil {
			w.CloseWithError(err)
			return
		}
		if len(buf) > 0 {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
		w.Close()
	}()
	tag, err := conn.Conn().PgConn().CopyFrom(ctx, r, `COPY `+ident+` (`+list+`) FROM STDIN WITH (FORMAT binary, FREEZE)`)
	_ = r.Close()
	<-written
	if err != nil {
		return tag.RowsAffected(), fmt.Errorf("loadseed: copy into %s: %w", table, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return tag.RowsAffected(), fmt.Errorf("loadseed: commit %s: %w", table, err)
	}
	return tag.RowsAffected(), nil
}

// encodeCopyValue appends value as a binary COPY field (its length, then
// its encoding for the column's type oid), as pgx's own COPY does
// (values.go encodeCopyValue): a value its type cannot encode directly is
// encoded through its text form.
func encodeCopyValue(m *pgtype.Map, buf []byte, oid uint32, value any) ([]byte, error) {
	at := len(buf)
	buf = append(buf, 0xff, 0xff, 0xff, 0xff) // NULL unless encoded below
	encoded, err := m.Encode(oid, pgtype.BinaryFormatCode, value, buf)
	if err != nil {
		text, ok := value.(string)
		if !ok {
			raw, textErr := m.Encode(oid, pgtype.TextFormatCode, value, nil)
			if textErr != nil {
				return nil, errors.Join(err, textErr)
			}
			text = string(raw)
		}
		var scanned any
		if scanErr := m.Scan(oid, pgtype.TextFormatCode, []byte(text), &scanned); scanErr != nil {
			return nil, errors.Join(err, scanErr)
		}
		if encoded, err = m.Encode(oid, pgtype.BinaryFormatCode, scanned, buf); err != nil {
			return nil, err
		}
	}
	if encoded == nil { // NULL
		return buf, nil
	}
	n := len(encoded) - at - 4
	encoded[at], encoded[at+1], encoded[at+2], encoded[at+3] = byte(n>>24), byte(n>>16), byte(n>>8), byte(n)
	return encoded, nil
}
