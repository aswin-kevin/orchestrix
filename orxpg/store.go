package orxpg

import (
	"context"
	"errors"
	"fmt"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/backend/kv"
	"github.com/jackc/pgx/v5"
)

// MaxKeyBytes is the longest key a Store accepts, in bytes. Longer keys
// fail with an error wrapping kv.ErrKeyTooLong.
const MaxKeyBytes = 1024

// Store is a kv.Store and kv.Stater backed by PostgreSQL, bound to one
// tenant. Over a pool it is safe for concurrent use (over a *pgx.Conn or
// pgx.Tx it is not; see DB), and many processes may share its table;
// concurrent Puts of one key leave the last one written. Create one with
// NewStore and pass it to kv.New.
type Store struct {
	db     DB
	tenant string

	sqlGet, sqlStat, sqlPut, sqlDelete, sqlList string
}

var (
	_ kv.Store  = (*Store)(nil)
	_ kv.Stater = (*Store)(nil)
)

// NewStore returns a Store for tenantID over db, usually a *pgxpool.Pool;
// over a pgx.Tx its reads and writes are the transaction's. The tables
// must exist (see Migrate). It fails for a nil db, an invalid tenant id
// (wrapping ErrInvalidTenantID) or an invalid Config.TablePrefix.
func NewStore(db DB, tenantID string, cfg Config) (*Store, error) {
	if db == nil {
		return nil, errNilDB
	}
	if err := checkTenant(tenantID); err != nil {
		return nil, err
	}
	prefix, err := cfg.prefix()
	if err != nil {
		return nil, err
	}
	t := tablesFor(prefix).kv
	where := ` WHERE tenant_id = $1 AND namespace = $2 AND key = $3`
	return &Store{
		db:      db,
		tenant:  tenantID,
		sqlGet:  `SELECT value, mod_time FROM ` + t + where,
		sqlStat: `SELECT size, mod_time FROM ` + t + where,
		sqlPut: `INSERT INTO ` + t + ` (tenant_id, namespace, key, value, size, mod_time)
VALUES ($1, $2, $3, $4, $5, clock_timestamp())
ON CONFLICT (tenant_id, namespace, key) DO UPDATE
SET value = EXCLUDED.value, size = EXCLUDED.size, mod_time = EXCLUDED.mod_time`,
		sqlDelete: `DELETE FROM ` + t + where,
		// key >= $3 lets the primary key index start at the prefix;
		// starts_with matches it literally, unlike LIKE.
		sqlList: `SELECT key, size, mod_time FROM ` + t + `
WHERE tenant_id = $1 AND namespace = $2 AND key >= $3 AND starts_with(key, $3)
ORDER BY key`,
	}, nil
}

// TenantID returns the tenant the Store is bound to.
func (s *Store) TenantID() string { return s.tenant }

func checkNamespace(namespace string) error {
	switch {
	case namespace == "":
		return errors.New("orxpg: empty namespace")
	case len(namespace) > kv.MaxNamespaceBytes:
		return fmt.Errorf("orxpg: namespace longer than %d bytes", kv.MaxNamespaceBytes)
	case !validText(namespace):
		return errors.New("orxpg: namespace is not valid UTF-8 or contains NUL")
	}
	return nil
}

func checkKey(key string) error {
	switch {
	case len(key) > MaxKeyBytes:
		return fmt.Errorf("orxpg: key of %d bytes: %w", len(key), kv.ErrKeyTooLong)
	case !validText(key):
		return fmt.Errorf("orxpg: key %q is not valid UTF-8 or contains NUL", key)
	}
	return nil
}

// check validates the context, namespace and key of a call.
func check(ctx context.Context, namespace, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkNamespace(namespace); err != nil {
		return err
	}
	return checkKey(key)
}

// Get returns a copy of the value stored under key.
func (s *Store) Get(ctx context.Context, namespace, key string) (kv.Item, error) {
	if err := check(ctx, namespace, key); err != nil {
		return kv.Item{}, err
	}
	var item kv.Item
	err := s.db.QueryRow(ctx, s.sqlGet, s.tenant, namespace, key).Scan(&item.Value, &item.ModTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return kv.Item{}, backend.ErrNotFound
	}
	if err != nil {
		return kv.Item{}, fmt.Errorf("orxpg: get %q: %w", key, err)
	}
	if item.Value == nil {
		item.Value = []byte{}
	}
	return item, nil
}

// Stat returns key's size and modification time without reading its value.
func (s *Store) Stat(ctx context.Context, namespace, key string) (kv.ItemInfo, error) {
	if err := check(ctx, namespace, key); err != nil {
		return kv.ItemInfo{}, err
	}
	info := kv.ItemInfo{Key: key}
	err := s.db.QueryRow(ctx, s.sqlStat, s.tenant, namespace, key).Scan(&info.Size, &info.ModTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return kv.ItemInfo{}, backend.ErrNotFound
	}
	if err != nil {
		return kv.ItemInfo{}, fmt.Errorf("orxpg: stat %q: %w", key, err)
	}
	return info, nil
}

// Put stores value under key, stamping its modification time with the
// database clock.
func (s *Store) Put(ctx context.Context, namespace, key string, value []byte) error {
	if err := check(ctx, namespace, key); err != nil {
		return err
	}
	if value == nil {
		value = []byte{} // NULL would violate NOT NULL
	}
	if _, err := s.db.Exec(ctx, s.sqlPut, s.tenant, namespace, key, value, int64(len(value))); err != nil {
		return fmt.Errorf("orxpg: put %q: %w", key, err)
	}
	return nil
}

// Delete removes key; a missing key is not an error.
func (s *Store) Delete(ctx context.Context, namespace, key string) error {
	if err := check(ctx, namespace, key); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, s.sqlDelete, s.tenant, namespace, key); err != nil {
		return fmt.Errorf("orxpg: delete %q: %w", key, err)
	}
	return nil
}

// List returns every key of namespace starting with prefix, in byte order.
// The prefix matches literally.
func (s *Store) List(ctx context.Context, namespace, prefix string) ([]kv.ItemInfo, error) {
	if err := check(ctx, namespace, prefix); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, s.sqlList, s.tenant, namespace, prefix)
	if err != nil {
		return nil, fmt.Errorf("orxpg: list %q: %w", prefix, err)
	}
	var out []kv.ItemInfo
	for rows.Next() {
		var info kv.ItemInfo
		if err := rows.Scan(&info.Key, &info.Size, &info.ModTime); err != nil {
			rows.Close()
			return nil, fmt.Errorf("orxpg: list %q: %w", prefix, err)
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orxpg: list %q: %w", prefix, err)
	}
	return out, nil
}
