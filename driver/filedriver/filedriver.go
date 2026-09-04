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
			driver.ListObjects,
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
		if os.IsNotExist(err) {
			// A prefix nothing is under is an empty level rather than
			// an error: in an object store there is nothing there to
			// be missing, and the two have to answer alike.
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
	return info.Size(), nil
}

// path resolves an object name inside the root, refusing anything that could
// escape it.
func (d *Driver) path(object string) (string, error) {
	if object == "" || object == "." || object == ".." ||
		strings.ContainsRune(object, os.PathSeparator) || strings.ContainsRune(object, '/') {
		return "", fmt.Errorf("%w: %q", ErrBadObject, object)
	}
	return filepath.Join(d.root, object), nil
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
