package kv

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aswin-kevin/orchestrix/backend"
)

const (
	// maxEncodedName is the longest file name common file systems accept.
	maxEncodedName = 255
	tempPrefix     = ".tmp-"
	dirMode        = 0o700
)

// DirStore is a Store kept on disk, so it survives restarts and is shared by
// processes. Every namespace and key is base64url-encoded into a single path
// component, so no namespace or key can name a path outside the store's
// directory. Writes are atomic; across processes the last write wins.
type DirStore struct {
	dir string
	now func() time.Time
}

var _ Store = (*DirStore)(nil)

var enc = base64.RawURLEncoding

// NewDirStore keeps each namespace in <dir>/<enc(namespace)>/ and each key in
// one file named enc(key), enc = base64.RawURLEncoding; dir is created 0o700,
// files 0o600; writes are atomic (os.CreateTemp in the namespace dir, then
// os.Rename); names starting with "." are temp files and never listed.
func NewDirStore(dir string, opts ...StoreOption) (*DirStore, error) {
	if dir == "" {
		return nil, errors.New("kv: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("kv: %w", err)
	}
	if err := os.MkdirAll(abs, dirMode); err != nil {
		return nil, fmt.Errorf("kv: create store directory: %w", err)
	}
	return &DirStore{dir: abs, now: newStoreOptions(opts).now}, nil
}

func (d *DirStore) nsDir(namespace string) (string, error) {
	if err := checkNamespace(namespace); err != nil {
		return "", err
	}
	return filepath.Join(d.dir, enc.EncodeToString([]byte(namespace))), nil
}

// checkKey enforces the length limits; the encoding makes every other key
// safe.
func checkKey(key string) error {
	if key == "" {
		return errors.New("kv: empty key")
	}
	if len(key) > MaxKeyBytes || enc.EncodedLen(len(key)) > maxEncodedName {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrKeyTooLong, len(key), MaxKeyBytes)
	}
	return nil
}

// Get returns the value stored under key.
func (d *DirStore) Get(ctx context.Context, namespace, key string) (Item, error) {
	if err := ctx.Err(); err != nil {
		return Item{}, err
	}
	nsDir, err := d.nsDir(namespace)
	if err != nil {
		return Item{}, err
	}
	if checkKey(key) != nil {
		// Such a key can never have been stored.
		return Item{}, fmt.Errorf("kv: get %s: %w", key, backend.ErrNotFound)
	}
	f, err := os.Open(filepath.Join(nsDir, enc.EncodeToString([]byte(key))))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Item{}, fmt.Errorf("kv: get %s: %w", key, backend.ErrNotFound)
		}
		return Item{}, fmt.Errorf("kv: get %s: %w", key, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err != nil {
		return Item{}, fmt.Errorf("kv: get %s: %w", key, err)
	}
	value, err := io.ReadAll(f)
	if err != nil {
		return Item{}, fmt.Errorf("kv: get %s: %w", key, err)
	}
	return Item{Value: value, ModTime: info.ModTime()}, nil
}

// Put atomically replaces the value under key: it writes a temp file in the
// namespace directory and renames it over the key's file.
func (d *DirStore) Put(ctx context.Context, namespace, key string, value []byte) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	nsDir, err := d.nsDir(namespace)
	if err != nil {
		return err
	}
	if err := checkKey(key); err != nil {
		return fmt.Errorf("kv: put %s: %w", key, err)
	}
	if err := os.MkdirAll(nsDir, dirMode); err != nil {
		return fmt.Errorf("kv: put %s: %w", key, err)
	}
	tmp, err := os.CreateTemp(nsDir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("kv: put %s: %w", key, err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(value); err != nil {
		return fmt.Errorf("kv: put %s: %w", key, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("kv: put %s: %w", key, err)
	}
	stamp := d.now()
	if err = os.Chtimes(tmp.Name(), stamp, stamp); err != nil {
		return fmt.Errorf("kv: put %s: %w", key, err)
	}
	if err = os.Rename(tmp.Name(), filepath.Join(nsDir, enc.EncodeToString([]byte(key)))); err != nil {
		return fmt.Errorf("kv: put %s: %w", key, err)
	}
	return nil
}

// Delete removes key; a missing key is not an error.
func (d *DirStore) Delete(ctx context.Context, namespace, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	nsDir, err := d.nsDir(namespace)
	if err != nil {
		return err
	}
	if checkKey(key) != nil {
		return nil
	}
	err = os.Remove(filepath.Join(nsDir, enc.EncodeToString([]byte(key))))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("kv: delete %s: %w", key, err)
	}
	return nil
}

// List returns every key of namespace starting with prefix. Temp files and
// foreign names in the namespace directory are ignored.
func (d *DirStore) List(ctx context.Context, namespace, prefix string) ([]ItemInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nsDir, err := d.nsDir(namespace)
	if err != nil {
		return nil, err
	}
	if len(prefix) > MaxKeyBytes {
		return nil, fmt.Errorf("kv: list %s: %w: %d bytes, limit %d", prefix, ErrKeyTooLong, len(prefix), MaxKeyBytes)
	}
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("kv: list %s: %w", prefix, err)
	}
	var out []ItemInfo
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		raw, err := enc.DecodeString(e.Name())
		if err != nil || !strings.HasPrefix(string(raw), prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue // removed concurrently, or not ours
		}
		out = append(out, ItemInfo{Key: string(raw), Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}
