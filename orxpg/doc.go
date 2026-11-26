// Package orxpg stores Orchestrix checkpoints and kv data in PostgreSQL, so
// workers on several machines can share threads and workspaces.
//
// Create the tables once (Migrate is idempotent), then build a Checkpointer
// and a Store for one tenant:
//
//	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
//	if err != nil {
//		log.Fatal(err)
//	}
//	if err := orxpg.Migrate(ctx, pool, orxpg.Config{}); err != nil {
//		log.Fatal(err)
//	}
//	cp, err := orxpg.NewCheckpointer(pool, "acme", orxpg.Config{})
//	if err != nil {
//		log.Fatal(err)
//	}
//	a, err := orchestrix.New("anthropic:claude-sonnet-4-5",
//		orchestrix.WithCheckpointer(cp))
//
// Checkpointer implements checkpoint.Checkpointer, Lister, Deleter and
// Leaser; Store implements kv.Store and kv.Stater, for use with kv.New.
//
// Both run their statements on a DB, which a *pgxpool.Pool, a *pgx.Conn
// and a pgx.Tx all satisfy. Over a pgx.Tx the reads and writes are part of
// the host's transaction and vanish if it rolls back; a Checkpointer or
// Store over a *pgx.Conn or a pgx.Tx is not safe for concurrent use.
//
// A Checkpointer used by an agent must sit over a pool (*pgxpool.Pool):
// the agent renews its thread's lease from a background goroutine while
// saves run, which is concurrent use, and over a pgx.Tx a lease is
// invisible to other processes until the transaction commits, so it
// excludes no one. A *pgx.Conn or a pgx.Tx suits only direct,
// single-goroutine use, such as tools, tests and migrations.
//
// # Tenancy
//
// The tenant id is fixed when a Checkpointer or Store is built and is part
// of every primary key and every WHERE clause, so one tenant's values never
// see another's threads, leases or keys. That separation is enforced by
// this package, not by the database: when tenants share a database and
// reach it with their own credentials, add row-level security as well.
//
// # Schema
//
// Migrate (or the DDL that Schema returns) creates three tables, named with
// the prefix "orx_" unless Config.TablePrefix sets another, in the first
// schema of the connection's search_path:
//
//   - orx_threads holds one row per thread: its state as bytea, the exact
//     bytes of state.State.MarshalJSON (never jsonb, which reorders keys),
//     the number of saves, a generation token and the time of the latest
//     save.
//   - orx_thread_leases holds the leases of checkpoint.Leaser. They live in
//     their own table because a run takes its thread's lease before the
//     thread's first save, when the thread has no row in orx_threads, and
//     because Delete must not drop a running owner's lease.
//   - orx_kv holds kv values with their size and modification time, so
//     Stat and List do not read values. Keys use COLLATE "C" (byte order).
//
// Thread ids also use COLLATE "C", so List pages in the byte order of Go
// strings, the order of checkpoint.Paginate. The index orx_threads_list on
// (tenant_id, updated_at DESC, thread_id) serves that keyset order.
// Releases up to v0.9 created orx_threads_updated without thread_id; the
// next Migrate builds orx_threads_list and drops the old index (only from
// the schema the tables are in). Building it blocks writes to orx_threads
// until it finishes; for a large table, run CREATE INDEX CONCURRENTLY with
// the same name and columns before upgrading, and Migrate finds it in
// place.
//
// # Versions
//
// A thread's version is its generation token and its save count,
// "<generation>:<saves>". The generation is a random token chosen when the
// thread's row is created, so a thread saved again after Delete never hands
// out a version of the deleted one. Every Save is one conditional
// statement: an INSERT ... ON CONFLICT DO NOTHING for the zero version, an
// UPDATE ... WHERE generation and saves match otherwise. When it changes no
// row, Save returns an error wrapping checkpoint.ErrConflict.
//
// # Leases
//
// Acquire, renew and release are each one statement and use the database
// clock, so the clocks of the workers do not matter. Acquire inserts the
// lease or takes over a row that the same owner holds or that has expired;
// when another owner holds an unexpired lease it changes nothing and
// returns an error wrapping checkpoint.ErrLeaseHeld. Renew only extends a
// row the owner holds that has not expired, and otherwise returns an error
// wrapping checkpoint.ErrLeaseHeld: an expired lease is never taken back
// by renew, since another worker may have held it in between. Release
// deletes the row only while the owner still holds it. A lease row left by
// a crashed worker stays until its thread's lease is next taken.
//
// # Times
//
// Updated times of threads and modification times of kv values come from
// the database clock, at the microsecond precision of timestamptz.
package orxpg
