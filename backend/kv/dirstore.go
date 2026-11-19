package kv

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
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
	tempPrefix = ".tmp-"
	dirMode    = 0o700
	// staleTempAge is how old a stray temp file must be before NewDirStore
	// removes it as the leftover of a crashed write.
	staleTempAge = time.Hour
	// maxHeaderBytes bounds the key header at the front of every file.
	maxHeaderBytes = binary.MaxVarintLen64 + MaxKeyBytes
)

// nsEncoding names namespace directories: lowercase unpadded base32hex, whose
// alphabet is safe on case-insensitive file systems.
var nsEncoding = base32.HexEncoding.WithPadding(base32.NoPadding)

// errCorrupt marks a key file whose header is unreadable.
var errCorrupt = errors.New("corrupt key file")

// DirStore is a Store kept on disk, so it survives restarts and is shared by
// processes. A namespace is a directory named by its lowercase base32hex
// encoding and a key is a file named by the lowercase hex SHA-256 of the key,
// so no namespace or key can name a path outside the store's directory and
// names never differ only in case. Each file holds uvarint(len(key)) + key +
// value; the stored key is what List reports and what Get verifies. Writes
// are atomic; across processes the last write wins (on Windows a concurrent
// Put from another process can fail with an access error).
type DirStore struct {
	dir string
	now func() time.Time
}

var _ Store = (*DirStore)(nil)

// NewDirStore keeps each namespace in <dir>/<base32hex(namespace)>/ and each
// key in one file named hex(sha256(key)) holding uvarint(len(key)) + key +
// value. The dir is created 0o700 (an existing directory keeps its mode),
// files are 0o600, and writes are atomic (os.CreateTemp in the namespace dir,
// fsync, then os.Rename). Names starting with "." are temp files and never
// listed; ones over an hour old are removed here.
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
	d := &DirStore{dir: abs, now: newStoreOptions(opts).now}
	d.removeStaleTemps()
	return d, nil
}

// removeStaleTemps deletes old temp files; failures are ignored because the
// files are harmless. The cutoff uses the real clock, not the store's: a test
// or skewed clock must not make another writer's in-flight temp look stale.
func (d *DirStore) removeStaleTemps() {
	cutoff := time.Now().Add(-staleTempAge)
	namespaces, _ := os.ReadDir(d.dir)
	for _, ns := range namespaces {
		if !ns.IsDir() {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(d.dir, ns.Name()))
		for _, f := range files {
			if !strings.HasPrefix(f.Name(), tempPrefix) {
				continue
			}
			if info, err := f.Info(); err == nil && info.ModTime().Before(cutoff) {
				_ = os.Remove(filepath.Join(d.dir, ns.Name(), f.Name()))
			}
		}
	}
}

func (d *DirStore) nsDir(namespace string) (string, error) {
	if err := checkNamespace(namespace); err != nil {
		return "", err
	}
	return filepath.Join(d.dir, strings.ToLower(nsEncoding.EncodeToString([]byte(namespace)))), nil
}

// fileName returns the file name of key.
func fileName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// checkKey enforces the length limit; hashing makes every other key safe.
func checkKey(key string) error {
	if key == "" {
		return ownError{errors.New("kv: empty key")}
	}
	if len(key) > MaxKeyBytes {
		return fmt.Errorf("%w: %d bytes, limit %d", ErrKeyTooLong, len(key), MaxKeyBytes)
	}
	return nil
}

// parseHeader splits the stored key off the front of head, returning it and
// the header length.
func parseHeader(head []byte) (key string, n int, err error) {
	size, w := binary.Uvarint(head)
	if w <= 0 || size == 0 || size > MaxKeyBytes || uint64(len(head)-w) < size {
		return "", 0, errCorrupt
	}
	n = w + int(size)
	return string(head[w:n]), n, nil
}

// readHeader reads the key header of an open key file.
func readHeader(f *os.File) (key string, n int, err error) {
	head := make([]byte, maxHeaderBytes)
	got, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", 0, err
	}
	return parseHeader(head[:got])
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
		return Item{}, backend.ErrNotFound // such a key can never have been stored
	}
	f, err := os.Open(filepath.Join(nsDir, fileName(key)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Item{}, backend.ErrNotFound
		}
		return Item{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Item{}, err
	}
	if err := checkRegular(info); err != nil {
		return Item{}, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return Item{}, err
	}
	stored, n, err := parseHeader(raw)
	if err != nil {
		return Item{}, fmt.Errorf("%w: unreadable key header", err)
	}
	if stored != key {
		return Item{}, backend.ErrNotFound // a hash collision or a forged file
	}
	return Item{Value: raw[n:], ModTime: info.ModTime()}, nil
}

// stat returns key's metadata without reading its value.
func (d *DirStore) stat(ctx context.Context, namespace, key string) (ItemInfo, error) {
	if err := ctx.Err(); err != nil {
		return ItemInfo{}, err
	}
	nsDir, err := d.nsDir(namespace)
	if err != nil {
		return ItemInfo{}, err
	}
	if checkKey(key) != nil {
		return ItemInfo{}, backend.ErrNotFound
	}
	f, err := os.Open(filepath.Join(nsDir, fileName(key)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ItemInfo{}, backend.ErrNotFound
		}
		return ItemInfo{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ItemInfo{}, err
	}
	if err := checkRegular(info); err != nil {
		return ItemInfo{}, err
	}
	stored, n, err := readHeader(f)
	if errors.Is(err, errCorrupt) {
		return ItemInfo{}, fmt.Errorf("%w: unreadable key header", err)
	}
	if err != nil {
		return ItemInfo{}, fmt.Errorf("read key header: %w", err)
	}
	if stored != key {
		return ItemInfo{}, backend.ErrNotFound
	}
	return ItemInfo{Key: key, Size: info.Size() - int64(n), ModTime: info.ModTime()}, nil
}

// checkRegular fails for a key file that is not a regular file (a directory
// put there by hand, say), which is not a corrupt value.
func checkRegular(info fs.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("key file %s is not a regular file", info.Name())
	}
	return nil
}

// Put atomically replaces the value under key: it writes a temp file in the
// namespace directory, syncs it, and renames it over the key's file.
func (d *DirStore) Put(ctx context.Context, namespace, key string, value []byte) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	nsDir, err := d.nsDir(namespace)
	if err != nil {
		return err
	}
	if err := checkKey(key); err != nil {
		return err
	}
	if err := os.MkdirAll(nsDir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(nsDir, tempPrefix+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	var body bytes.Buffer
	body.Write(binary.AppendUvarint(nil, uint64(len(key))))
	body.WriteString(key)
	body.Write(value)
	if _, err = tmp.Write(body.Bytes()); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	stamp := d.now()
	if err = os.Chtimes(tmp.Name(), stamp, stamp); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(nsDir, fileName(key))); err != nil {
		return err
	}
	syncDir(nsDir)
	return nil
}

// syncDir makes a rename durable. It is best effort: the rename already
// happened, and some platforms (Windows) cannot sync directories.
func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
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
	err = os.Remove(filepath.Join(nsDir, fileName(key)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// List returns every key of namespace starting with prefix. It reads only
// each file's header; temp files, unreadable files and files whose name does
// not match their stored key are ignored.
func (d *DirStore) List(ctx context.Context, namespace, prefix string) ([]ItemInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nsDir, err := d.nsDir(namespace)
	if err != nil {
		return nil, err
	}
	if len(prefix) > MaxKeyBytes {
		return nil, fmt.Errorf("%w: prefix is %d bytes, limit %d", ErrKeyTooLong, len(prefix), MaxKeyBytes)
	}
	entries, err := os.ReadDir(nsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []ItemInfo
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if info, ok := readEntry(filepath.Join(nsDir, e.Name()), e.Name()); ok && strings.HasPrefix(info.Key, prefix) {
			out = append(out, info)
		}
	}
	return out, nil
}

// readEntry reads one key file's metadata; ok is false for anything that is
// not a well-formed key file under its own name.
func readEntry(p, name string) (info ItemInfo, ok bool) {
	f, err := os.Open(p)
	if err != nil {
		return ItemInfo{}, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return ItemInfo{}, false
	}
	key, n, err := readHeader(f)
	if err != nil || fileName(key) != name {
		return ItemInfo{}, false
	}
	return ItemInfo{Key: key, Size: st.Size() - int64(n), ModTime: st.ModTime()}, true
}
