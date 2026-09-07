// Package filedriver is a driver over a directory.
//
// It exists to give the contract something real to be exercised against, and
// it is the simplest case of register-in-place: files already in the directory
// are readable objects without anything rewriting them.
package filedriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/mayur-tolexo/forebay/driver"
)

// ErrBadObject rejects a name that is not a single ordinary file.
var ErrBadObject = errors.New("filedriver: object name is not a plain file name")

// Driver serves objects from one directory.
type Driver struct{ root string }

// New points a driver at a directory.
func New(root string) (*Driver, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("filedriver: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("filedriver: %s is not a directory", root)
	}
	return &Driver{root: root}, nil
}

// Declare says what a directory can do.
//
// Snapshot and clone are absent because a plain filesystem cannot do them
// without copying, and a clone that copies is not a clone. Declaring them and
// emulating would be the silent degradation the contract forbids.
func (d *Driver) Declare() driver.Declaration {
	return driver.Declaration{
		Contract: 1,
		Capabilities: []driver.Capability{
			driver.ReadRange, driver.ObjectSize, driver.WriteObject, driver.DeleteObject,
			driver.ListObjects, driver.WriteStream,
		},
	}
}

// List returns one level under a prefix, in name order.
//
// A directory here is a directory, which is the easy case: the store this
// serves is a filesystem, so the level a caller asked for is one readdir. An
// object store has no directories and has to derive them, and both have to
// answer the same shape or a namespace built on one would not work on the
// other.
func (d *Driver) List(ctx context.Context, prefix, after string, limit int) ([]driver.Entry, error) {
	dir := d.root
	if prefix != "" {
		p, err := d.path(prefix)
		if err != nil {
			return nil, err
		}
		dir = p
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			// A prefix nothing is under is an empty level rather than
			// an error: in an object store there is nothing there to
			// be missing, and the two have to answer alike. A prefix
			// that names an object is the same case, because an
			// object store has nothing under one either.
			return nil, nil
		}
		return nil, fmt.Errorf("filedriver: listing %s: %w", prefix, err)
	}

	// Sorted, because a caller pages with the last name it saw and an order
	// that changed between calls would skip or repeat.
	sort.Slice(names, func(i, j int) bool { return names[i].Name() < names[j].Name() })

	out := make([]driver.Entry, 0, limit)
	for _, e := range names {
		if e.Name() <= after {
			continue
		}
		if len(out) == limit {
			break
		}
		entry := driver.Entry{Name: e.Name(), Dir: e.IsDir()}
		if !entry.Dir {
			if info, err := e.Info(); err == nil {
				entry.Bytes = info.Size()
			}
		}
		out = append(out, entry)
	}
	return out, ctx.Err()
}

// SizeOf reports how many bytes an object holds.
func (d *Driver) SizeOf(ctx context.Context, object string) (int64, error) {
	p, err := d.path(object)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(p)
	if err != nil {
		return 0, fmt.Errorf("filedriver: %w", err)
	}
	if info.IsDir() {
		// A directory is a level in the namespace, not an object. Its
		// size is the filesystem's own bookkeeping, and returning it
		// tells a caller that separates the two by asking how large
		// something is that every level is a file.
		return 0, fmt.Errorf("%w: %q is a directory", ErrBadObject, object)
	}
	return info.Size(), nil
}

// path resolves an object key inside the root, refusing anything that could
// escape it.
//
// A key is slash-separated and may name more than one level, because an object
// store's keys do: a namespace built over one store has to walk the same way
// over the other, and a driver that took only single names could not serve it.
func (d *Driver) path(object string) (string, error) {
	if object == "" {
		return "", fmt.Errorf("%w: %q", ErrBadObject, object)
	}
	segments := strings.Split(object, "/")
	for _, s := range segments {
		// An empty segment is a leading, trailing or doubled slash, and
		// each of those names the same level by two spellings.
		if s == "" || s == "." || s == ".." ||
			strings.ContainsRune(s, os.PathSeparator) {
			return "", fmt.Errorf("%w: %q", ErrBadObject, object)
		}
	}
	return filepath.Join(append([]string{d.root}, segments...)...), nil
}

// ReadRange reads length bytes from offset.
//
// A range reaching past the end is an error rather than a short read: a caller
// that asked for a range and got fewer bytes cannot tell that from truncation.
func (d *Driver) ReadRange(ctx context.Context, object string, offset, length int64) ([]byte, error) {
	p, err := d.path(object)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("filedriver: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("filedriver: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %q is a directory", ErrBadObject, object)
	}
	if offset+length > info.Size() {
		return nil, fmt.Errorf("%w: %d bytes from %d, object is %d", driver.ErrRange, length, offset, info.Size())
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(io.NewSectionReader(f, offset, length), buf); err != nil {
		return nil, fmt.Errorf("filedriver: %w", err)
	}
	return buf, nil
}

// WriteObject creates an immutable object, refusing to replace one.
func (d *Driver) WriteObject(ctx context.Context, object string, data []byte) error {
	p, err := d.path(object)
	if err != nil {
		return err
	}
	// A key naming levels that do not exist yet is an ordinary key in an
	// object store, where the levels are not real. Here they are.
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("filedriver: %w", err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("filedriver: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("filedriver: %w", err)
	}
	return f.Close()
}

// WriteObjectFrom copies an object in from a reader without holding it.
//
// The size is checked against what arrived rather than trusted. A caller that
// said one number and sent another has written an object whose length is not
// what anything else in the system believes, and a short checkpoint that
// reports success is the failure RFC-0013 exists to prevent.
func (d *Driver) WriteObjectFrom(ctx context.Context, object string, src io.ReadSeeker, size int64) error {
	p, err := d.path(object)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("filedriver: %w", err)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("filedriver: %w", err)
	}
	// One byte past the declared size, so a source holding more is caught
	// rather than truncated to fit. A caller that said the wrong number has
	// an object that is not what it described either way, and the short and
	// the long case are the same mistake.
	n, err := io.Copy(f, io.LimitReader(src, size+1))
	if err != nil {
		f.Close()
		os.Remove(p)
		return fmt.Errorf("filedriver: %w", err)
	}
	if n != size {
		// Removed, not left wrong. A partial object under the name the caller
		// asked for is worse than none: the next reader cannot tell it from a
		// whole one.
		f.Close()
		os.Remove(p)
		return fmt.Errorf("filedriver: %s was declared %d bytes and the source holds %s",
			object, size, atLeast(n, size))
	}
	// Flushed before the write is acknowledged. This driver is the durable
	// side of a checkpoint, and an acknowledgement the page cache could still
	// lose is the acknowledgement RFC-0013 refuses to offer.
	if err := syncFile(f); err != nil {
		f.Close()
		os.Remove(p)
		return fmt.Errorf("filedriver: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(p)
		return fmt.Errorf("filedriver: %w", err)
	}
	return ctx.Err()
}

// syncFile flushes a file to the device.
//
// A variable so a test can see that it was called. Whether the bytes reached
// the platter is not something a test can observe, and dropping the call is
// something an edit can do silently, so what is pinned is the call.
var syncFile = func(f *os.File) error { return f.Sync() }

// atLeast renders how much a source held, saying "more than" when the read
// stopped one byte past the declared size rather than at the source's end.
func atLeast(read, size int64) string {
	if read > size {
		return fmt.Sprintf("more than %d", size)
	}
	return fmt.Sprintf("%d", read)
}

// DeleteObject removes one.
func (d *Driver) DeleteObject(ctx context.Context, object string) error {
	p, err := d.path(object)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		return fmt.Errorf("filedriver: %w", err)
	}
	return nil
}

// SnapshotObject is not something a directory does.
func (d *Driver) SnapshotObject(ctx context.Context, object string) (string, error) {
	return "", fmt.Errorf("%w: %s", driver.ErrNotSupported, driver.Snapshot)
}

// CloneObject is not something a directory does. Copying the bytes would
// satisfy the signature and defeat the point of asking for a clone.
func (d *Driver) CloneObject(ctx context.Context, from, to string) error {
	return fmt.Errorf("%w: %s", driver.ErrNotSupported, driver.Clone)
}
