package orxpg_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/aswin-kevin/orchestrix/orxpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dsnEnv names the variable holding the connection string of the test
// database. Tests that need it are skipped when it is unset.
const dsnEnv = "ORX_TEST_PG_DSN"

func randomName(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// testSchema creates an empty schema holding migrated tables and returns
// its name; the schema is dropped when the test ends.
func testSchema(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	schema := emptySchema(t)
	if err := orxpg.Migrate(ctx, newPool(t, schema), orxpg.Config{}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return schema
}

// emptySchema creates an empty schema and returns its name; the schema is
// dropped when the test ends.
func emptySchema(t *testing.T) string {
	t.Helper()
	ctx := t.Context()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s is not set", dsnEnv)
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)
	schema := randomName("orxpg_test_")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Errorf("connect for cleanup: %v", err)
			return
		}
		defer conn.Close(context.Background())
		if _, err := conn.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})
	return schema
}

// newPool opens a pool whose search_path is schema, closed when the test
// ends (before the schema is dropped). schema may list several schemas,
// already quoted where needed.
func newPool(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	ctx := t.Context()
	cfg, err := pgxpool.ParseConfig(os.Getenv(dsnEnv))
	if err != nil {
		t.Fatalf("parse %s: %v", dsnEnv, err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newConn opens a single connection whose search_path is schema, closed
// when the test ends.
func newConn(t *testing.T, schema string) *pgx.Conn {
	t.Helper()
	ctx := t.Context()
	cfg, err := pgx.ParseConfig(os.Getenv(dsnEnv))
	if err != nil {
		t.Fatalf("parse %s: %v", dsnEnv, err)
	}
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func newCheckpointer(t *testing.T, db orxpg.DB, tenant string) *orxpg.Checkpointer {
	t.Helper()
	c, err := orxpg.NewCheckpointer(db, tenant, orxpg.Config{})
	if err != nil {
		t.Fatalf("NewCheckpointer: %v", err)
	}
	return c
}

func newStore(t *testing.T, db orxpg.DB, tenant string) *orxpg.Store {
	t.Helper()
	s, err := orxpg.NewStore(db, tenant, orxpg.Config{})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}
