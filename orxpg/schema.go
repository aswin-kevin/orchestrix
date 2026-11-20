package orxpg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultTablePrefix starts the name of every table this package uses,
// unless WithTablePrefix sets another prefix.
const DefaultTablePrefix = "orx_"

// MaxTenantIDBytes is the longest accepted tenant id, in bytes.
const MaxTenantIDBytes = 200

// migrateLockKey is the advisory lock that serializes concurrent Migrate
// calls, so two processes creating the tables at once do not collide.
const migrateLockKey = 0x6f72787067 // "orxpg"

// ErrInvalidTenantID is returned for an empty tenant id, one longer than
// MaxTenantIDBytes, or one that is not valid UTF-8 or contains NUL.
var ErrInvalidTenantID = errors.New("orxpg: invalid tenant id")

type options struct {
	prefix string
}

// Option configures Migrate, Schema, NewCheckpointer and NewStore.
type Option func(*options)

// WithTablePrefix sets the prefix of the table names (DefaultTablePrefix
// when unset). It must be made of lowercase ASCII letters, digits and
// underscores, start with a letter or underscore, and be at most 40 bytes.
// Pass the same prefix to Migrate and to the constructors.
func WithTablePrefix(prefix string) Option {
	return func(o *options) { o.prefix = prefix }
}

func newOptions(opts []Option) (options, error) {
	o := options{prefix: DefaultTablePrefix}
	for _, opt := range opts {
		opt(&o)
	}
	if err := checkPrefix(o.prefix); err != nil {
		return options{}, err
	}
	return o, nil
}

func checkPrefix(p string) error {
	if p == "" || len(p) > 40 {
		return fmt.Errorf("orxpg: table prefix %q must be 1 to 40 bytes", p)
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return fmt.Errorf("orxpg: table prefix %q may hold only lowercase letters, digits and underscores, and must not start with a digit", p)
		}
	}
	return nil
}

// tables holds the quoted table names for one prefix.
type tables struct {
	threads, leases, kv string
	prefix              string
}

func tablesFor(prefix string) tables {
	q := func(name string) string { return pgx.Identifier{prefix + name}.Sanitize() }
	return tables{threads: q("threads"), leases: q("thread_leases"), kv: q("kv"), prefix: prefix}
}

// ddl returns the statements that create the tables.
func (t tables) ddl() []string {
	idx := pgx.Identifier{t.prefix + "threads_updated"}.Sanitize()
	return []string{
		`CREATE TABLE IF NOT EXISTS ` + t.threads + ` (
    tenant_id  text COLLATE "C" NOT NULL,
    thread_id  text COLLATE "C" NOT NULL,
    generation text        NOT NULL,
    saves      bigint      NOT NULL,
    state      bytea       NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, thread_id)
)`,
		`CREATE INDEX IF NOT EXISTS ` + idx + ` ON ` + t.threads + ` (tenant_id, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS ` + t.leases + ` (
    tenant_id  text COLLATE "C" NOT NULL,
    thread_id  text COLLATE "C" NOT NULL,
    owner      text        NOT NULL,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, thread_id)
)`,
		`CREATE TABLE IF NOT EXISTS ` + t.kv + ` (
    tenant_id  text COLLATE "C" NOT NULL,
    namespace  text COLLATE "C" NOT NULL,
    key        text COLLATE "C" NOT NULL,
    value      bytea       NOT NULL,
    size       bigint      NOT NULL,
    mod_time   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, namespace, key)
)`,
	}
}

// Schema returns the SQL that Migrate runs, for use with a migration tool.
// Every statement is idempotent. It panics on an invalid WithTablePrefix.
func Schema(opts ...Option) string {
	o, err := newOptions(opts)
	if err != nil {
		panic(err)
	}
	return strings.Join(tablesFor(o.prefix).ddl(), ";\n\n") + ";\n"
}

// Migrate creates the tables when they do not exist, in the first schema of
// the connection's search_path. It is safe to call on every start and from
// several processes at once.
func Migrate(ctx context.Context, pool *pgxpool.Pool, opts ...Option) error {
	if pool == nil {
		return errors.New("orxpg: nil pool")
	}
	o, err := newOptions(opts)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("orxpg: migrate: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLockKey)); err != nil {
		return fmt.Errorf("orxpg: migrate: %w", err)
	}
	for _, stmt := range tablesFor(o.prefix).ddl() {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("orxpg: migrate: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("orxpg: migrate: %w", err)
	}
	return nil
}

func checkTenant(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: empty", ErrInvalidTenantID)
	case len(id) > MaxTenantIDBytes:
		return fmt.Errorf("%w: %d bytes exceeds the %d byte limit", ErrInvalidTenantID, len(id), MaxTenantIDBytes)
	case !validText(id):
		return fmt.Errorf("%w: not valid UTF-8 or contains NUL", ErrInvalidTenantID)
	}
	return nil
}

// validText reports whether s can be stored in a text column: valid UTF-8
// without NUL.
func validText(s string) bool {
	return utf8.ValidString(s) && strings.IndexByte(s, 0) < 0
}
