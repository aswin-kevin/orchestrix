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
//	if err := orxpg.Migrate(ctx, pool); err != nil {
//		log.Fatal(err)
//	}
//	cp, err := orxpg.NewCheckpointer(pool, "acme")
//	if err != nil {
//		log.Fatal(err)
//	}
//	a, err := orchestrix.New("anthropic:claude-sonnet-4-5", orchestrix.WithCheckpointer(cp))
//
// Checkpointer implements checkpoint.Checkpointer and checkpoint.Leaser;
// Store implements kv.Store and kv.Stater, for use with kv.New.
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
// the prefix "orx_" unless WithTablePrefix sets another, in the first
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
