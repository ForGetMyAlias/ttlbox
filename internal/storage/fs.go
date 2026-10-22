package storage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// FS keeps blobs on a local (or shared, e.g. NFS) directory, sharded by id
// prefix. A blob is visible only once its .json sidecar exists, and both are
// written via temp file + rename, so readers never see partial uploads.
type FS struct {
	dir  string
	cap  int64 // total bytes allowed, 0 = unlimited
	used atomic.Int64
}

func NewFS(dir string, capBytes int64) (*FS, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f := &FS{dir: dir, cap: capBytes}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return err
		}
		if m, err := f.readMeta(strings.TrimSuffix(d.Name(), ".json")); err == nil {
			f.used.Add(m.Size)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	_, err = f.Sweep(context.Background())
	return f, err
}

func (f *FS) base(id string) string { return filepath.Join(f.dir, id[:2], id) }

func (f *FS) readMeta(id string) (Meta, error) {
	var m Meta
	if !ValidID(id) {
		return m, ErrNotFound
	}
	b, err := os.ReadFile(f.base(id) + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return m, ErrNotFound
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, ErrNotFound
	}
	return m, nil
}

// writeAtomic writes via a temp file in the same directory, then renames.
func writeAtomic(path string, r io.Reader) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*") // 0600
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return n, err
}

func (f *FS) Put(_ context.Context, id string, m Meta, r io.Reader) (Meta, error) {
	if !ValidID(id) {
		return m, errInvalidID
	}
	if f.cap > 0 && f.used.Load() >= f.cap {
		return m, ErrFull
	}
	base := f.base(id)
	if err := os.MkdirAll(filepath.Dir(base), 0o700); err != nil {
		return m, err
	}
	n, err := writeAtomic(base+".bin", r)
	if err != nil {
		return m, err
	}
	undo := func() { os.Remove(base + ".bin"); f.used.Add(-n) }
	if f.used.Add(n) > f.cap && f.cap > 0 {
		undo()
		return m, ErrFull
	}
	m.Size = n
	mj, _ := json.Marshal(m)
	if _, err := writeAtomic(base+".json", strings.NewReader(string(mj))); err != nil {
		undo()
		return m, err
	}
	return m, nil
}

func (f *FS) Stat(_ context.Context, id string) (Meta, error) {
	m, err := f.readMeta(id)
	if err != nil {
		return Meta{}, err
	}
	if time.Now().After(m.Expires) {
		f.remove(id, m)
		return Meta{}, ErrNotFound
	}
	return m, nil
}

func (f *FS) Open(ctx context.Context, id string) (Meta, io.ReadSeekCloser, error) {
	m, err := f.Stat(ctx, id)
	if err != nil {
		return m, nil, err
	}
	file, err := os.Open(f.base(id) + ".bin")
	if errors.Is(err, os.ErrNotExist) {
		return m, nil, ErrNotFound
	}
	return m, file, err
}

func (f *FS) Delete(_ context.Context, id string) error {
	m, err := f.readMeta(id)
	if err != nil {
		return err
	}
	f.remove(id, m)
	return nil
}

// remove drops the sidecar first (hiding the item), and only the caller that
// actually removed it adjusts the usage counter.
func (f *FS) remove(id string, m Meta) {
	base := f.base(id)
	if os.Remove(base+".json") == nil {
		f.used.Add(-m.Size)
	}
	os.Remove(base + ".bin")
}

func (f *FS) Sweep(ctx context.Context) (int, error) {
	removed := 0
	stale := func(d fs.DirEntry) bool {
		info, err := d.Info()
		return err == nil && time.Since(info.ModTime()) > time.Hour
	}
	err := filepath.WalkDir(f.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		name := d.Name()
		switch {
		case strings.HasPrefix(name, ".tmp-"):
			if stale(d) {
				os.Remove(p)
			}
		case strings.HasSuffix(name, ".bin"): // orphan from a crash before the sidecar landed
			if _, err := os.Stat(strings.TrimSuffix(p, ".bin") + ".json"); errors.Is(err, os.ErrNotExist) && stale(d) {
				os.Remove(p)
			}
		case strings.HasSuffix(name, ".json"):
			id := strings.TrimSuffix(name, ".json")
			if m, err := f.readMeta(id); err == nil && time.Now().After(m.Expires) {
				f.remove(id, m)
				removed++
			}
		}
		return nil
	})
	return removed, err
}
