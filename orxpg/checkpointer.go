package orxpg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aswin-kevin/orchestrix/checkpoint"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Checkpointer is a checkpoint.Checkpointer and checkpoint.Leaser backed by
// PostgreSQL, bound to one tenant. It is safe for concurrent use, and any
// number of processes may share its tables: saves are conditional and
// leases exclude runs across all of them. Create one with NewCheckpointer.
type Checkpointer struct {
	pool   *pgxpool.Pool
	tenant string

	sqlCreate, sqlUpdate, sqlLoad, sqlList, sqlDelete string
	sqlLease, sqlRenew, sqlRelease                    string
}

var (
	_ checkpoint.Checkpointer = (*Checkpointer)(nil)
	_ checkpoint.Lister       = (*Checkpointer)(nil)
	_ checkpoint.Deleter      = (*Checkpointer)(nil)
	_ checkpoint.Leaser       = (*Checkpointer)(nil)
)

// NewCheckpointer returns a Checkpointer for tenantID over pool. The tables
// must exist (see Migrate). It fails for a nil pool or an invalid tenant id
// (wrapping ErrInvalidTenantID).
func NewCheckpointer(pool *pgxpool.Pool, tenantID string, opts ...Option) (*Checkpointer, error) {
	if pool == nil {
		return nil, errors.New("orxpg: nil pool")
	}
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	o, err := newOptions(opts)
	if err != nil {
		return nil, err
	}
	t := tablesFor(o.prefix)
	return &Checkpointer{
		pool:   pool,
		tenant: tenantID,
		sqlCreate: `INSERT INTO ` + t.threads + ` (tenant_id, thread_id, generation, saves, state, updated_at)
VALUES ($1, $2, $3, 1, $4, clock_timestamp())
ON CONFLICT (tenant_id, thread_id) DO NOTHING
RETURNING saves`,
		sqlUpdate: `UPDATE ` + t.threads + `
SET state = $5, saves = saves + 1, updated_at = clock_timestamp()
WHERE tenant_id = $1 AND thread_id = $2 AND generation = $3 AND saves = $4
RETURNING saves`,
		sqlLoad: `SELECT generation, saves, state FROM ` + t.threads + `
WHERE tenant_id = $1 AND thread_id = $2`,
		sqlList: `SELECT thread_id, generation, saves, updated_at FROM ` + t.threads + `
WHERE tenant_id = $1
AND ($2::timestamptz IS NULL OR updated_at < $2 OR (updated_at = $2 AND thread_id > $3))
ORDER BY updated_at DESC, thread_id LIMIT $4`,
		sqlDelete: `DELETE FROM ` + t.threads + ` WHERE tenant_id = $1 AND thread_id = $2`,
		sqlLease: `INSERT INTO ` + t.leases + ` AS l (tenant_id, thread_id, owner, expires_at)
VALUES ($1, $2, $3, clock_timestamp() + $4::bigint * interval '1 microsecond')
ON CONFLICT (tenant_id, thread_id) DO UPDATE
SET owner = EXCLUDED.owner, expires_at = EXCLUDED.expires_at
WHERE l.owner = EXCLUDED.owner OR l.expires_at <= clock_timestamp()
RETURNING l.owner`,
		sqlRenew: `UPDATE ` + t.leases + `
SET expires_at = clock_timestamp() + $4::bigint * interval '1 microsecond'
WHERE tenant_id = $1 AND thread_id = $2 AND owner = $3 AND expires_at > clock_timestamp()
RETURNING owner`,
		sqlRelease: `DELETE FROM ` + t.leases + ` WHERE tenant_id = $1 AND thread_id = $2 AND owner = $3`,
	}, nil
}

// TenantID returns the tenant the Checkpointer is bound to.
func (c *Checkpointer) TenantID() string { return c.tenant }

// Save stores the JSON encoding of s as the thread's latest state when the
// thread is at version expected, in one conditional statement.
func (c *Checkpointer) Save(ctx context.Context, threadID string, s *state.State, expected checkpoint.Version) (checkpoint.Version, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return "", err
	}
	if s == nil {
		return "", errors.New("orxpg: save: nil state")
	}
	data, err := s.MarshalJSON()
	if err != nil {
		return "", fmt.Errorf("orxpg: encode state: %w", err)
	}
	var (
		gen   string
		saves int64
	)
	if expected == "" {
		gen = checkpoint.NewGeneration()
		err = c.pool.QueryRow(ctx, c.sqlCreate, c.tenant, threadID, gen, data).Scan(&saves)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("orxpg: save: %w: thread %q already exists", checkpoint.ErrConflict, threadID)
		}
	} else {
		var want int64
		var ok bool
		if gen, want, ok = checkpoint.ParseVersion(expected); !ok {
			return "", fmt.Errorf("orxpg: save: %w: thread %q cannot be at version %q", checkpoint.ErrConflict, threadID, expected)
		}
		err = c.pool.QueryRow(ctx, c.sqlUpdate, c.tenant, threadID, gen, want, data).Scan(&saves)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("orxpg: save: %w: thread %q is not at version %q", checkpoint.ErrConflict, threadID, expected)
		}
	}
	if err != nil {
		return "", fmt.Errorf("orxpg: save %q: %w", threadID, err)
	}
	return checkpoint.FormatVersion(gen, saves), nil
}

// Load decodes the thread's latest state into a fresh State.
func (c *Checkpointer) Load(ctx context.Context, threadID string) (*state.State, checkpoint.Version, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return nil, "", err
	}
	var (
		gen   string
		saves int64
		data  []byte
	)
	err := c.pool.QueryRow(ctx, c.sqlLoad, c.tenant, threadID).Scan(&gen, &saves, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", checkpoint.ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("orxpg: load %q: %w", threadID, err)
	}
	s := state.New()
	if err := s.UnmarshalJSON(data); err != nil {
		return nil, "", fmt.Errorf("orxpg: decode state of %q: %w", threadID, err)
	}
	return s, checkpoint.FormatVersion(gen, saves), nil
}

// List returns one page of the tenant's threads, newest first. It is a
// keyset query: each page starts strictly after the (updated_at, thread_id)
// position its cursor names, so paging cost does not grow with the offset.
func (c *Checkpointer) List(ctx context.Context, opts checkpoint.ListOptions) (checkpoint.ListPage, error) {
	if err := ctx.Err(); err != nil {
		return checkpoint.ListPage{}, err
	}
	limit := opts.Limit
	switch {
	case limit < 0:
		return checkpoint.ListPage{}, fmt.Errorf("checkpoint: negative list limit %d", limit)
	case limit == 0:
		limit = checkpoint.DefaultListLimit
	}
	var (
		after   *time.Time
		afterID string
	)
	if opts.Cursor != "" {
		updated, id, err := checkpoint.ParseListCursor(opts.Cursor)
		if err != nil {
			return checkpoint.ListPage{}, err
		}
		after, afterID = &updated, id
	}
	rows, err := c.pool.Query(ctx, c.sqlList, c.tenant, after, afterID, limit+1)
	if err != nil {
		return checkpoint.ListPage{}, fmt.Errorf("orxpg: list: %w", err)
	}
	defer rows.Close()
	page := checkpoint.ListPage{Threads: []checkpoint.ThreadInfo{}}
	for rows.Next() {
		var (
			id, gen string
			saves   int64
			updated time.Time
		)
		if err := rows.Scan(&id, &gen, &saves, &updated); err != nil {
			return checkpoint.ListPage{}, fmt.Errorf("orxpg: list: %w", err)
		}
		page.Threads = append(page.Threads, checkpoint.ThreadInfo{ID: id, Updated: updated, Saves: int(saves), Version: checkpoint.FormatVersion(gen, saves)})
	}
	if err := rows.Err(); err != nil {
		return checkpoint.ListPage{}, fmt.Errorf("orxpg: list: %w", err)
	}
	if len(page.Threads) > limit {
		page.Threads = page.Threads[:limit]
		page.Next = checkpoint.ListCursor(page.Threads[limit-1])
	}
	return page, nil
}

// Delete removes the thread. Its lease, if any, is left to its owner.
func (c *Checkpointer) Delete(ctx context.Context, threadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return err
	}
	tag, err := c.pool.Exec(ctx, c.sqlDelete, c.tenant, threadID)
	if err != nil {
		return fmt.Errorf("orxpg: delete %q: %w", threadID, err)
	}
	if tag.RowsAffected() == 0 {
		return checkpoint.ErrNotFound
	}
	return nil
}

// Acquire implements checkpoint.Leaser. The lease holds across every
// process that shares the tables; its expiry is measured by the database
// clock, at microsecond precision.
func (c *Checkpointer) Acquire(ctx context.Context, threadID, owner string, ttl time.Duration) (*checkpoint.Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkpoint.ValidThreadID(threadID); err != nil {
		return nil, err
	}
	if owner == "" {
		return nil, errors.New("orxpg: lease owner is empty")
	}
	if !validText(owner) {
		return nil, fmt.Errorf("orxpg: lease owner %q is not valid UTF-8 or contains NUL", owner)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("orxpg: lease ttl %v is not positive", ttl)
	}
	micros := max(ttl.Microseconds(), 1)
	// run executes a lease statement that returns a row only when owner
	// holds the lease afterwards.
	run := func(ctx context.Context, sql string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var got string
		err := c.pool.QueryRow(ctx, sql, c.tenant, threadID, owner, micros).Scan(&got)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("orxpg: %w: thread %q", checkpoint.ErrLeaseHeld, threadID)
		}
		if err != nil {
			return fmt.Errorf("orxpg: lease %q: %w", threadID, err)
		}
		return nil
	}
	if err := run(ctx, c.sqlLease); err != nil {
		return nil, err
	}
	renew := func(ctx context.Context) error { return run(ctx, c.sqlRenew) }
	release := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := c.pool.Exec(ctx, c.sqlRelease, c.tenant, threadID, owner); err != nil {
			return fmt.Errorf("orxpg: release lease %q: %w", threadID, err)
		}
		return nil
	}
	return checkpoint.NewLease(threadID, owner, renew, release), nil
}
