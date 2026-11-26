package orxpg_test

import (
	"errors"
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
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCheckpointerConformance(t *testing.T) {
	checkpointtest.Run(t, func(t *testing.T) checkpoint.Checkpointer {
		return newCheckpointer(t, newPool(t, testSchema(t)), "tenant")
	})
}

func TestStoreConformance(t *testing.T) {
	kvtest.Run(t, func(t *testing.T) kv.Store {
		return newStore(t, newPool(t, testSchema(t)), "tenant")
	})
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
			if err := orxpg.Migrate(ctx, newPool(t, schema)); err != nil {
				t.Errorf("Migrate again: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestTablePrefix(t *testing.T) {
	pool := newPool(t, testSchema(t))
	if err := orxpg.Migrate(ctx, pool, orxpg.WithTablePrefix("app_")); err != nil {
		t.Fatal(err)
	}
	c, err := orxpg.NewCheckpointer(pool, "tenant", orxpg.WithTablePrefix("app_"))
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

// TestOffline covers argument checks that need no database.
func TestOffline(t *testing.T) {
	if _, err := orxpg.NewCheckpointer(nil, "t"); err == nil {
		t.Error("NewCheckpointer(nil) succeeded")
	}
	if _, err := orxpg.NewStore(nil, "t"); err == nil {
		t.Error("NewStore(nil) succeeded")
	}
	if err := orxpg.Migrate(ctx, nil); err == nil {
		t.Error("Migrate(nil) succeeded")
	}
	pool, err := pgxpool.New(ctx, "postgres://localhost:1/none") // never connected
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, id := range []string{"", "a\x00b", "bad\xff", strings.Repeat("x", orxpg.MaxTenantIDBytes+1)} {
		if _, err := orxpg.NewCheckpointer(pool, id); !errors.Is(err, orxpg.ErrInvalidTenantID) {
			t.Errorf("NewCheckpointer(%.12q) = %v, want ErrInvalidTenantID", id, err)
		}
		if _, err := orxpg.NewStore(pool, id); !errors.Is(err, orxpg.ErrInvalidTenantID) {
			t.Errorf("NewStore(%.12q) = %v, want ErrInvalidTenantID", id, err)
		}
	}
	for _, p := range []string{"", "Orx_", "1orx", "orx-", `orx"`, strings.Repeat("a", 41)} {
		if _, err := orxpg.NewCheckpointer(pool, "t", orxpg.WithTablePrefix(p)); err == nil {
			t.Errorf("WithTablePrefix(%q) accepted", p)
		}
	}
	if s := orxpg.Schema(); !strings.Contains(s, `CREATE TABLE IF NOT EXISTS "orx_threads"`) || strings.Contains(s, "jsonb") {
		t.Errorf("Schema() = %s", s)
	}
	c, err := orxpg.NewCheckpointer(pool, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Save(ctx, "a\x00", stateWith("x"), ""); !errors.Is(err, checkpoint.ErrInvalidThreadID) {
		t.Errorf("Save with an invalid id = %v", err)
	}
	s, err := orxpg.NewStore(pool, "t")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "ns", "/"+strings.Repeat("k", orxpg.MaxKeyBytes), nil); !errors.Is(err, kv.ErrKeyTooLong) {
		t.Errorf("Put with a long key = %v, want kv.ErrKeyTooLong", err)
	}
}
