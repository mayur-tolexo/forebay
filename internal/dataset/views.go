package dataset

import (
	"fmt"
	"path"
	"strings"
)

// FilePath renders a reference as the path the file view serves it at, under
// the root an export is mounted from.
//
// The dataset, version and object in that order, which is the same order the
// object key uses, so a user reading a path and a user reading a key are
// looking at the same three names in the same places.
func (r Ref) FilePath(root string) string {
	return path.Join(root, r.Dataset, r.Version, r.Object)
}

// ObjectKey renders a reference as the key the object view serves it at.
//
// The bucket is not part of it. A bucket names an export rather than a
// dataset, and putting it here would make the same bytes have two keys when
// the same dataset is exported twice.
func (r Ref) ObjectKey() string { return r.String() }

// ErrNotUnderRoot reports a path outside the export it was resolved against.
// It is refused rather than trimmed, because a path that escapes its export is
// a caller asking for something the export does not hold.
var ErrNotUnderRoot = fmt.Errorf("dataset: path is not under the export root")

// ParseObjectKey turns an object key back into what a user named.
//
// The object may itself contain slashes, since a shard laid out in directories
// is still one object, so the split takes the first two names and leaves the
// rest.
func ParseObjectKey(key string) (Ref, error) {
	parts := strings.SplitN(strings.TrimPrefix(key, separator), separator, 3)
	if len(parts) < 3 {
		return Ref{}, fmt.Errorf("%w, got %q", ErrEmpty, key)
	}
	r := Ref{Dataset: parts[0], Version: parts[1], Object: parts[2]}
	if err := r.Validate(); err != nil {
		return Ref{}, err
	}
	return r, nil
}

// ParseFilePath turns a served path back into what a user named, which is what
// makes the two views one thing rather than two that agree by convention.
func ParseFilePath(root, p string) (Ref, error) {
	clean, cleanRoot := path.Clean(p), path.Clean(root)
	if cleanRoot != "." && cleanRoot != separator {
		rest, ok := strings.CutPrefix(clean, cleanRoot+separator)
		if !ok {
			return Ref{}, fmt.Errorf("%w: %q is not under %q", ErrNotUnderRoot, p, root)
		}
		clean = rest
	}
	return ParseObjectKey(clean)
}
