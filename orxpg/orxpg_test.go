package orxpg_test

import (
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/backendtest"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/aswin-kevin/orchestrix/backend/kv/kvtest"
	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/checkpoint/checkpointtest"
	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/orxpg"
	"github.com/aswin-kevin/orchestrix/state"
)

func TestCheckpointerConformance(t *testing.T) {
	checkpointtest.Run(t, func(t *testing.T) checkpoint.Checkpointer {
		return newCheckpointer(t, newPool(t, testSchema(t)), "tenant")
	})
}

func TestStoreConformance(t *testing.T) {
	// The database stamps ModTime with its own clock, which may differ
	// from the test machine's.
	kvtest.RunWith(t, func(t *testing.T) kv.Store {
		return newStore(t, newPool(t, testSchema(t)), "tenant")
	}, kvtest.Options{ModTimeTolerance: time.Minute})
}

func TestBackendConformance(t *testing.T) {
	backendtest.Run(t, func(t *testing.T) backend.Backend {
		b, err := kv.New(newStore(t, newPool(t, testSchema(t)), "tenant"), "ns")
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

func stateWith(texts ...string) *state.State {
	s := state.New()
	for _, text := range texts {
		s.AppendMessages(llm.UserText(text))
	}
	return s
}

// TestSeparatePoolsOneSaveWins races two Checkpointers on separate pools
// over one thread: in every round exactly one save succeeds.
func TestSeparatePoolsOneSaveWins(t *testing.T) {
	schema := testSchema(t)
	cps := []*orxpg.Checkpointer{
		newCheckpointer(t, newPool(t, schema), "tenant"),
		newCheckpointer(t, newPool(t, schema), "tenant"),
	}
	base, err := cps[0].Save(ctx, "shared", stateWith("base"), "")
	if err != nil {
		t.Fatal(err)
	}
	for round := range 25 {
		var (
			wg        sync.WaitGroup
			mu        sync.Mutex
			winners   []checkpoint.Version
			conflicts int
		)
		start := make(chan struct{})
		for i, c := range cps {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				v, err := c.Save(ctx, "shared", stateWith("base", strings.Repeat("w", i+1)), base)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					winners = append(winners, v)
				case errors.Is(err, checkpoint.ErrConflict):
					conflicts++
				default:
					t.Errorf("round %d: Save: %v", round, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if len(winners) != 1 || conflicts != 1 {
			t.Fatalf("round %d: %d saves won and %d conflicted, want 1 and 1", round, len(winners), conflicts)
		}
		for _, c := range cps {
			if _, v, err := c.Load(ctx, "shared"); err != nil || v != winners[0] {
				t.Fatalf("round %d: Load = %q, %v, want %q", round, v, err, winners[0])
			}
		}
		base = winners[0]
	}
}

// TestSeparatePoolsOneLeaseWins races two Checkpointers on separate pools
// for one thread's lease.
func TestSeparatePoolsOneLeaseWins(t *testing.T) {
	schema := testSchema(t)
	cps := []*orxpg.Checkpointer{
		newCheckpointer(t, newPool(t, schema), "tenant"),
		newCheckpointer(t, newPool(t, schema), "tenant"),
	}
	for round := range 25 {
		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			releases []*checkpoint.Lease
			held     int
		)
		start := make(chan struct{})
		for i, c := range cps {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				lease, err := c.Acquire(ctx, "shared", strings.Repeat("o", i+1), time.Minute)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					releases = append(releases, lease)
				case errors.Is(err, checkpoint.ErrLeaseHeld):
					held++
				default:
					t.Errorf("round %d: Acquire: %v", round, err)
				}
			}()
		}
		close(start)
		wg.Wait()
		if len(releases) != 1 || held != 1 {
			t.Fatalf("round %d: %d owners got the lease and %d were refused, want 1 and 1", round, len(releases), held)
		}
		if err := releases[0].Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLeaseBeforeFirstSave(t *testing.T) {
	c := newCheckpointer(t, newPool(t, testSchema(t)), "tenant")
	lease, err := c.Acquire(ctx, "new", "a", time.Minute)
	if err != nil {
		t.Fatalf("Acquire on a thread with no checkpoint: %v", err)
	}
	if _, err := c.Save(ctx, "new", stateWith("1"), ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire(ctx, "new", "b", time.Minute); !errors.Is(err, checkpoint.ErrLeaseHeld) {
		t.Fatalf("Delete dropped the lease: Acquire by another owner = %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTenantIsolation(t *testing.T) {
	pool := newPool(t, testSchema(t))
	a := newCheckpointer(t, pool, "tenant-a")
	b := newCheckpointer(t, pool, "tenant-b")

	va, err := a.Save(ctx, "t", stateWith("a's"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Load(ctx, "t"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("B loaded A's thread: %v", err)
	}
	if page, err := b.List(ctx, checkpoint.ListOptions{}); err != nil || len(page.Threads) != 0 {
		t.Fatalf("B listed %+v, %v; want nothing", page, err)
	}
	if err := b.Delete(ctx, "t"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("B deleted A's thread: %v", err)
	}
	if _, err := b.Save(ctx, "t", stateWith("b's"), va); !errors.Is(err, checkpoint.ErrConflict) {
		t.Fatalf("B saved over A's version: %v", err)
	}
	if _, err := b.Save(ctx, "t", stateWith("b's"), ""); err != nil {
		t.Fatalf("B creating its own thread of the same id: %v", err)
	}
	got, v, err := a.Load(ctx, "t")
	if err != nil || v != va || got.Messages()[0].Text() != "a's" {
		t.Fatalf("A's thread changed: %q at %q, %v", got.Messages()[0].Text(), v, err)
	}
	if page, err := a.List(ctx, checkpoint.ListOptions{}); err != nil || len(page.Threads) != 1 {
		t.Fatalf("A listed %+v, %v; want its one thread", page, err)
	}
	rel, err := a.Acquire(ctx, "t", "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer rel.Release(ctx)
	if relB, err := b.Acquire(ctx, "t", "other", time.Minute); err != nil {
		t.Fatalf("A's lease blocked B: %v", err)
	} else {
		relB.Release(ctx)
	}

	sa := newStore(t, pool, "tenant-a")
	sb := newStore(t, pool, "tenant-b")
	if err := sa.Put(ctx, "ns", "/k", []byte("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Get(ctx, "ns", "/k"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("B got A's key: %v", err)
	}
	if _, err := sb.Stat(ctx, "ns", "/k"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("B stat A's key: %v", err)
	}
	if infos, err := sb.List(ctx, "ns", ""); err != nil || len(infos) != 0 {
		t.Fatalf("B listed %+v, %v", infos, err)
	}
	if err := sb.Delete(ctx, "ns", "/k"); err != nil {
		t.Fatal(err)
	}
	if item, err := sa.Get(ctx, "ns", "/k"); err != nil || string(item.Value) != "a" {
		t.Fatalf("B's Delete reached A's key: %q, %v", item.Value, err)
	}
}

func TestStateBytesKept(t *testing.T) {
	pool := newPool(t, testSchema(t))
	c := newCheckpointer(t, pool, "tenant")
	s := state.New()
	if err := s.UnmarshalJSON([]byte(`{"version":1,"messages":[],"values":{"z.key":{"b":2,"a":1},"a.key":"x"}}`)); err != nil {
		t.Fatal(err)
	}
	want, err := s.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Save(ctx, "t", s, ""); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT state FROM orx_threads WHERE thread_id = 't'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(want) {
		t.Fatalf("stored bytes differ from MarshalJSON:\n got %s\nwant %s", stored, want)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	schema := testSchema(t) // already migrated once
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := orxpg.Migrate(ctx, newPool(t, schema), orxpg.Config{}); err != nil {
				t.Errorf("Migrate again: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestTablePrefix(t *testing.T) {
	pool := newPool(t, testSchema(t))
	cfg := orxpg.Config{TablePrefix: "app_"}
	if err := orxpg.Migrate(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	c, err := orxpg.NewCheckpointer(pool, "tenant", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Save(ctx, "t", stateWith("x"), ""); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_threads`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("app_threads has %d rows, %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orx_threads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orx_threads has %d rows, %v", n, err)
	}
}

// saveThreads saves one thread per id, then gives every thread of the
// tenant the same updated_at, so their order rests on the tie-break alone.
func saveThreads(t *testing.T, db orxpg.DB, c *orxpg.Checkpointer, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := c.Save(ctx, id, stateWith(id), ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `UPDATE orx_threads SET updated_at = '2026-01-02 03:04:05.123456+00' WHERE tenant_id = $1`, c.TenantID()); err != nil {
		t.Fatal(err)
	}
}

// listAll pages through c with limit and returns each page's ids.
func listAll(t *testing.T, c *orxpg.Checkpointer, limit int) [][]string {
	t.Helper()
	var pages [][]string
	cursor := ""
	for range 100 {
		page, err := c.List(ctx, checkpoint.ListOptions{Limit: limit, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, th := range page.Threads {
			ids = append(ids, th.ID)
		}
		pages = append(pages, ids)
		if page.Next == "" {
			return pages
		}
		cursor = page.Next
	}
	t.Fatal("paging did not terminate")
	return nil
}

func TestListPagesTiesByThreadID(t *testing.T) {
	pool := newPool(t, testSchema(t))
	c := newCheckpointer(t, pool, "tenant")
	saveThreads(t, pool, c, "c", "e", "a", "d", "b")
	got := listAll(t, c, 2)
	want := [][]string{{"a", "b"}, {"c", "d"}, {"e"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("pages = %v, want %v", got, want)
	}
}

// TestListTieBreakIsByteOrder checks that the database orders ids exactly
// as checkpoint.Paginate does (Go byte order), whatever the database's
// default collation: uppercase before lowercase, punctuation by code point.
func TestListTieBreakIsByteOrder(t *testing.T) {
	pool := newPool(t, testSchema(t))
	c := newCheckpointer(t, pool, "tenant")
	ids := []string{"b", "B", "a", "_z", "A-1", "a1", "é", "Z"}
	saveThreads(t, pool, c, ids...)
	var infos []checkpoint.ThreadInfo
	at := time.Unix(1, 0)
	for _, id := range ids {
		infos = append(infos, checkpoint.ThreadInfo{ID: id, Updated: at})
	}
	for _, limit := range []int{1, 3} {
		var want [][]string
		cursor := ""
		for {
			p, err := checkpoint.Paginate(infos, checkpoint.ListOptions{Limit: limit, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			var page []string
			for _, th := range p.Threads {
				page = append(page, th.ID)
			}
			want = append(want, page)
			if p.Next == "" {
				break
			}
			cursor = p.Next
		}
		if got := listAll(t, c, limit); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("limit %d: pages = %v, want %v", limit, got, want)
		}
	}
}

// TestListHugeLimit checks that the largest Limit does not overflow the
// one-extra-row LIMIT the query fetches.
func TestListHugeLimit(t *testing.T) {
	c := newCheckpointer(t, newPool(t, testSchema(t)), "tenant")
	for _, id := range []string{"a", "b", "c"} {
		if _, err := c.Save(ctx, id, stateWith(id), ""); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.List(ctx, checkpoint.ListOptions{Limit: math.MaxInt})
	if err != nil || len(page.Threads) != 3 || page.Next != "" {
		t.Fatalf("List(MaxInt) = %+v, %v; want all 3 threads and no Next", page, err)
	}
}

// TestCheckpointerInsideTx runs a Checkpointer and a Store over a pgx.Tx:
// their writes are the transaction's, gone after a rollback.
func TestCheckpointerInsideTx(t *testing.T) {
	pool := newPool(t, testSchema(t))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	c := newCheckpointer(t, tx, "tenant")
	v, err := c.Save(ctx, "t", stateWith("x"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, got, err := c.Load(ctx, "t"); err != nil || got != v {
		t.Fatalf("Load inside the tx = %q, %v; want %q", got, err, v)
	}
	if page, err := c.List(ctx, checkpoint.ListOptions{}); err != nil || len(page.Threads) != 1 {
		t.Fatalf("List inside the tx = %+v, %v", page, err)
	}
	lease, err := c.Acquire(ctx, "t", "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Renew(ctx); err != nil {
		t.Fatal(err)
	}
	s := newStore(t, tx, "tenant")
	if err := s.Put(ctx, "ns", "/k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	outside := newCheckpointer(t, pool, "tenant")
	if _, _, err := outside.Load(ctx, "t"); !errors.Is(err, checkpoint.ErrNotFound) {
		t.Fatalf("Load after rollback = %v, want ErrNotFound", err)
	}
	if other, err := outside.Acquire(ctx, "t", "other", time.Minute); err != nil {
		t.Fatalf("the rolled-back lease still holds: %v", err)
	} else {
		_ = other.Release(ctx)
	}
	if _, err := newStore(t, pool, "tenant").Get(ctx, "ns", "/k"); !errors.Is(err, backend.ErrNotFound) {
		t.Fatalf("Get after rollback = %v, want ErrNotFound", err)
	}
}

// TestMigrateOverConn migrates over a single *pgx.Conn and over a pgx.Tx
// (where Migrate's own transaction becomes a savepoint).
func TestMigrateOverConn(t *testing.T) {
	conn := newConn(t, emptySchema(t))
	if err := orxpg.Migrate(ctx, conn, orxpg.Config{}); err != nil {
		t.Fatalf("Migrate over a conn: %v", err)
	}
	c := newCheckpointer(t, conn, "tenant")
	v, err := c.Save(ctx, "t", stateWith("x"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, got, err := c.Load(ctx, "t"); err != nil || got != v {
		t.Fatalf("Load = %q, %v; want %q", got, err, v)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	cfg := orxpg.Config{TablePrefix: "tx_"}
	if err := orxpg.Migrate(ctx, tx, cfg); err != nil {
		t.Fatalf("Migrate over a tx: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM tx_threads`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tx_threads after commit: %d rows, %v", n, err)
	}
}

// createV09Threads creates the threads table and list index the way
// releases up to v0.9 did, in the first schema of db's search_path.
func createV09Threads(t *testing.T, db orxpg.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE TABLE orx_threads (
    tenant_id  text COLLATE "C" NOT NULL,
    thread_id  text COLLATE "C" NOT NULL,
    generation text        NOT NULL,
    saves      bigint      NOT NULL,
    state      bytea       NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, thread_id)
)`,
		`CREATE INDEX orx_threads_updated ON orx_threads (tenant_id, updated_at DESC)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// indexes returns the index definitions of schema, keyed by index name.
func indexes(t *testing.T, db orxpg.DB, schema string) map[string]string {
	t.Helper()
	rows, err := db.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = $1`, schema)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatal(err)
		}
		out[name] = def
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMigrateReplacesListIndex migrates a schema made by an older release:
// the list index gains the thread_id tie-break, the old index is dropped
// and the stored threads stay.
func TestMigrateReplacesListIndex(t *testing.T) {
	schema := emptySchema(t)
	pool := newPool(t, schema)
	createV09Threads(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO orx_threads VALUES ('tenant', 'old', 'g', 1, '{"version":1,"messages":[],"values":{}}', now())`); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second run finds everything in place
		if err := orxpg.Migrate(ctx, pool, orxpg.Config{}); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	got := indexes(t, pool, schema)
	if _, ok := got["orx_threads_updated"]; ok {
		t.Errorf("the old index orx_threads_updated is still there: %v", got)
	}
	if def := got["orx_threads_list"]; !strings.Contains(def, "(tenant_id, updated_at DESC, thread_id)") {
		t.Errorf("orx_threads_list = %q, want it on (tenant_id, updated_at DESC, thread_id)", def)
	}
	for _, name := range []string{"orx_threads_pkey", "orx_thread_leases_pkey", "orx_kv_pkey"} {
		if _, ok := got[name]; !ok {
			t.Errorf("index %s missing: %v", name, got)
		}
	}
	if len(got) != 4 {
		t.Errorf("indexes = %v, want the 3 primary keys and orx_threads_list", got)
	}
	c := newCheckpointer(t, pool, "tenant")
	if _, v, err := c.Load(ctx, "old"); err != nil || v != checkpoint.FormatVersion("g", 1) {
		t.Fatalf("Load of a thread saved before the migration = %q, %v", v, err)
	}
}

// TestMigrateLeavesOtherSchemas checks that dropping the old list index
// touches only the schema Migrate creates tables in, even when a later
// schema of the search_path holds an older install.
func TestMigrateLeavesOtherSchemas(t *testing.T) {
	first, second := emptySchema(t), emptySchema(t)
	createV09Threads(t, newPool(t, second))
	pool := newPool(t, first+", "+second)
	if err := orxpg.Migrate(ctx, pool, orxpg.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := indexes(t, pool, second)["orx_threads_updated"]; !ok {
		t.Error("Migrate dropped the old index of a later schema in the search_path")
	}
	if _, ok := indexes(t, pool, first)["orx_threads_list"]; !ok {
		t.Error("Migrate did not create orx_threads_list in the first schema")
	}
}
