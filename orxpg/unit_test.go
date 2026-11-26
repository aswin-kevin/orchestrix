package orxpg_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/orxpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The pgx handles a host already holds all serve as a DB and a Beginner.
var (
	_ orxpg.DB       = (*pgxpool.Pool)(nil)
	_ orxpg.DB       = (*pgx.Conn)(nil)
	_ orxpg.DB       = pgx.Tx(nil)
	_ orxpg.Beginner = (*pgxpool.Pool)(nil)
	_ orxpg.Beginner = (*pgx.Conn)(nil)
	_ orxpg.Beginner = pgx.Tx(nil)
)

func TestSchemaDefault(t *testing.T) {
	s, err := orxpg.Schema(orxpg.Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`CREATE TABLE IF NOT EXISTS "orx_threads"`,
		`CREATE TABLE IF NOT EXISTS "orx_thread_leases"`,
		`CREATE TABLE IF NOT EXISTS "orx_kv"`,
		// The list index covers the keyset order, tie-break included.
		`CREATE INDEX IF NOT EXISTS "orx_threads_list" ON "orx_threads" (tenant_id, updated_at DESC, thread_id)`,
		// The index older releases created is dropped from this schema only.
		`DROP INDEX IF EXISTS %I.%I', current_schema(), 'orx_threads_updated'`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Schema lacks %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "jsonb") {
		t.Errorf("Schema stores jsonb:\n%s", s)
	}
}

func TestSchemaPrefix(t *testing.T) {
	s, err := orxpg.Schema(orxpg.Config{TablePrefix: "app_"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, `"app_threads_list" ON "app_threads"`) || !strings.Contains(s, `'app_threads_updated'`) || strings.Contains(s, "orx_") {
		t.Errorf("Schema(app_) =\n%s", s)
	}
}

func TestSchemaInvalidPrefix(t *testing.T) {
	for _, p := range []string{"Orx_", "1orx", "orx-", `orx"`, "orx'", strings.Repeat("a", 41)} {
		s, err := orxpg.Schema(orxpg.Config{TablePrefix: p})
		if err == nil || s != "" || !strings.HasPrefix(err.Error(), "orxpg: ") {
			t.Errorf("Schema(%q) = %q, %v; want an orxpg error", p, s, err)
		}
	}
}

func TestNewRejectsNilDB(t *testing.T) {
	// An untyped nil: a typed nil *pgxpool.Pool is a non-nil DB.
	if _, err := orxpg.NewCheckpointer(nil, "t", orxpg.Config{}); err == nil || err.Error() != "orxpg: nil db" {
		t.Errorf("NewCheckpointer(nil) = %v, want orxpg: nil db", err)
	}
	if _, err := orxpg.NewStore(nil, "t", orxpg.Config{}); err == nil || err.Error() != "orxpg: nil db" {
		t.Errorf("NewStore(nil) = %v, want orxpg: nil db", err)
	}
	if err := orxpg.Migrate(ctx, nil, orxpg.Config{}); err == nil || err.Error() != "orxpg: nil db" {
		t.Errorf("Migrate(nil) = %v, want orxpg: nil db", err)
	}
}

// offlinePool is a pool that never connects: building one does not dial.
func offlinePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, "postgres://localhost:1/none")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestNewRejectsInvalidArguments(t *testing.T) {
	pool := offlinePool(t)
	for _, id := range []string{"", "a\x00b", "bad\xff", strings.Repeat("x", orxpg.MaxTenantIDBytes+1)} {
		if _, err := orxpg.NewCheckpointer(pool, id, orxpg.Config{}); !errors.Is(err, orxpg.ErrInvalidTenantID) {
			t.Errorf("NewCheckpointer(%.12q) = %v, want ErrInvalidTenantID", id, err)
		}
		if _, err := orxpg.NewStore(pool, id, orxpg.Config{}); !errors.Is(err, orxpg.ErrInvalidTenantID) {
			t.Errorf("NewStore(%.12q) = %v, want ErrInvalidTenantID", id, err)
		}
	}
	for _, p := range []string{"Orx_", "1orx", "orx-", `orx"`, strings.Repeat("a", 41)} {
		cfg := orxpg.Config{TablePrefix: p}
		if _, err := orxpg.NewCheckpointer(pool, "t", cfg); err == nil {
			t.Errorf("NewCheckpointer accepted prefix %q", p)
		}
		if _, err := orxpg.NewStore(pool, "t", cfg); err == nil {
			t.Errorf("NewStore accepted prefix %q", p)
		}
		if err := orxpg.Migrate(ctx, pool, cfg); err == nil {
			t.Errorf("Migrate accepted prefix %q", p)
		}
	}
}

// TestOfflineChecks covers argument checks that fail before any statement
// reaches the database.
func TestOfflineChecks(t *testing.T) {
	pool := offlinePool(t)
	c, err := orxpg.NewCheckpointer(pool, "t", orxpg.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Save(ctx, "a\x00", stateWith("x"), ""); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
		t.Errorf("Save with an invalid id = %v", err)
	}
	_, err = c.List(ctx, checkpoint.ListOptions{Limit: -1})
	want := checkpoint.ListOptions{Limit: -1}
	if _, werr := want.PageLimit(); err == nil || err.Error() != werr.Error() {
		t.Errorf("List(Limit -1) = %v, want the checkpoint error %v", err, werr)
	}
	if _, err := c.List(ctx, checkpoint.ListOptions{Cursor: "!!"}); !errors.Is(err, checkpoint.ErrInvalidCursor) {
		t.Errorf("List with a bad cursor = %v, want ErrInvalidCursor", err)
	}
	s, err := orxpg.NewStore(pool, "t", orxpg.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "ns", "/"+strings.Repeat("k", orxpg.MaxKeyBytes), nil); !errors.Is(err, kv.ErrKeyTooLong) {
		t.Errorf("Put with a long key = %v, want kv.ErrKeyTooLong", err)
	}
}
