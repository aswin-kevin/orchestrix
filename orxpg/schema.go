package orxpg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// DefaultTablePrefix starts the name of every table and index this
// package uses, unless Config.TablePrefix sets another prefix.
const DefaultTablePrefix = "orx_"

// MaxTenantIDBytes is the longest accepted tenant id, in bytes.
const MaxTenantIDBytes = 200

// migrateLockKey is the advisory lock that serializes concurrent Migrate
// calls, so two processes creating the tables at once do not collide.
const migrateLockKey = 0x6f72787067 // "orxpg"

// ErrInvalidTenantID is returned for an empty tenant id, one longer than
// MaxTenantIDBytes, or one that is not valid UTF-8 or contains NUL.
var ErrInvalidTenantID = errors.New("orxpg: invalid tenant id")

func checkPrefix(p string) error {
	if len(p) > 40 {
		return fmt.Errorf("orxpg: table prefix %q is longer than 40 bytes", p)
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

// ddl returns the statements that create the tables and indexes. The
// prefix is validated, so it is safe inside a string literal.
func (t tables) ddl() []string {
	idx := pgx.Identifier{t.prefix + "threads_list"}.Sanitize()
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
		// The list index covers List's whole keyset order, tie-break
		// included. It replaces <prefix>threads_updated, which releases up
		// to v0.9 created without thread_id; the old index is dropped from
		// current_schema() only, the schema the tables are created in, so
		// another install later in the search_path keeps its own.
		`CREATE INDEX IF NOT EXISTS ` + idx + ` ON ` + t.threads + ` (tenant_id, updated_at DESC, thread_id)`,
		`DO $$
BEGIN
    EXECUTE format('DROP INDEX IF EXISTS %I.%I', current_schema(), '` + t.prefix + `threads_updated');
END
$$`,
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

// Schema returns the SQL that Migrate runs for cfg, for use with a
// migration tool. Every statement is idempotent. The output contains a
// DO $$ ... $$ block, which must run as a single statement: a migration
// tool that splits its input on ';' needs that block wrapped in its
// statement-block markers. It fails for an invalid Config.TablePrefix.
func Schema(cfg Config) (string, error) {
	prefix, err := cfg.prefix()
	if err != nil {
		return "", err
	}
	return strings.Join(tablesFor(prefix).ddl(), ";\n\n") + ";\n", nil
}

// Migrate creates the tables and indexes when they do not exist, in the
// first schema of the connection's search_path, and replaces the list
// index of older releases (see the package documentation). It runs in one
// transaction begun on db, so it is safe to call on every start and from
// several processes at once. It fails for a nil db or an invalid
// Config.TablePrefix.
func Migrate(ctx context.Context, db Beginner, cfg Config) error {
	if db == nil {
		return errNilDB
	}
	prefix, err := cfg.prefix()
	if err != nil {
		return err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("orxpg: migrate: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrateLockKey)); err != nil {
		return fmt.Errorf("orxpg: migrate: %w", err)
	}
	for _, stmt := range tablesFor(prefix).ddl() {
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
