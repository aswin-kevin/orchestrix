package orxpg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB runs the statements of a Checkpointer or Store. *pgxpool.Pool,
// *pgx.Conn and pgx.Tx all satisfy it. A pool is safe for concurrent use;
// a *pgx.Conn or a pgx.Tx is not, so a Checkpointer or Store built over one
// must not be used by several goroutines at once.
type DB interface {
	// Exec runs a statement that returns no rows.
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	// Query runs a statement that returns rows.
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	// QueryRow runs a statement that returns at most one row.
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Beginner starts the transaction Migrate runs in. *pgxpool.Pool,
// *pgx.Conn and pgx.Tx all satisfy it; over a pgx.Tx, Migrate's
// transaction is a savepoint and its changes commit with the outer
// transaction.
type Beginner interface {
	// Begin starts a transaction.
	Begin(ctx context.Context) (pgx.Tx, error)
}

// errNilDB is returned by Migrate and the constructors for a nil db.
var errNilDB = errors.New("orxpg: nil db")

// Config configures Schema, Migrate, NewCheckpointer and NewStore. The
// zero Config is valid. Pass the same Config to Migrate and to the
// constructors.
type Config struct {
	// TablePrefix starts the name of every table and index. It must be
	// made of lowercase ASCII letters, digits and underscores, start with
	// a letter or underscore, and be at most 40 bytes. Empty means
	// DefaultTablePrefix.
	TablePrefix string
}

// prefix returns the validated table prefix c selects.
func (c Config) prefix() (string, error) {
	p := c.TablePrefix
	if p == "" {
		p = DefaultTablePrefix
	}
	if err := checkPrefix(p); err != nil {
		return "", err
	}
	return p, nil
}
