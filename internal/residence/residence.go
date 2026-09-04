// Package residence decides where a dataset version's bytes may be.
//
// RFC-0025 builds this first and nothing else, because a residency breach is a
// disclosure that has already happened: deleting the copy afterwards does not
// undo it. A rule added after transfers have run is a rule that was not
// enforced for anything already moved.
package residence

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Region names a cluster or a regulatory area, whichever a deployment's rules
// are written in terms of.
type Region string

var (
	// ErrDenied reports a region a version may never enter.
	ErrDenied = errors.New("residence: this region is denied")
	// ErrNotAllowed reports a region outside the ones a version is confined
	// to.
	ErrNotAllowed = errors.New("residence: this region is not among those allowed")
	// ErrUnnamed reports a region nothing could name. It is refused rather
	// than permitted, on the same reasoning the rest of this project uses for
	// an unknown fact: an unknown never satisfies a requirement.
	ErrUnnamed = errors.New("residence: an unnamed region is not a destination")
)

// Policy is where a version's bytes may be.
//
// Two lists rather than one, because they answer different questions. Allowed
// confines a version to somewhere; Denied excludes it from somewhere while
// leaving the rest open, which is how a single prohibited jurisdiction is
// usually expressed.
type Policy struct {
	// Allowed confines the version to these regions. Empty means unconfined,
	// which is the ordinary case: most data has no residency requirement and
	// requiring one would make the rule something operators route around.
	Allowed []Region
	// Denied excludes these regions and wins over Allowed. A version permitted
	// by one rule and forbidden by another is forbidden, or adding a
	// permission could silently remove a prohibition.
	Denied []Region
}

// Permits reports whether a version under this policy may be in a region.
//
// The same question answers "may it move here" and "should it be here", so a
// rule tightened after a version moved reports what is now in breach instead
// of applying only to future transfers.
func (p Policy) Permits(r Region) error {
	if strings.TrimSpace(string(r)) == "" {
		return ErrUnnamed
	}
	for _, d := range p.Denied {
		if d == r {
			return fmt.Errorf("%w: %s", ErrDenied, r)
		}
	}
	if len(p.Allowed) == 0 {
		return nil
	}
	for _, a := range p.Allowed {
		if a == r {
			return nil
		}
	}
	return fmt.Errorf("%w: %s, allowed are %s", ErrNotAllowed, r, list(p.Allowed))
}

// Transfer reports whether a version may move from one region to another.
//
// Both ends are checked. A version that should not have been at the origin is
// a breach already, and letting it be the source of a transfer would spread
// one mistake rather than report it.
func Transfer(p Policy, from, to Region) error {
	if err := p.Permits(from); err != nil {
		return fmt.Errorf("origin %w", err)
	}
	if err := p.Permits(to); err != nil {
		return fmt.Errorf("destination %w", err)
	}
	return nil
}

// Breaches reports the regions a version is in that it may not be in, so a
// rule tightened after the fact says what is now wrong rather than only
// binding what happens next.
func Breaches(p Policy, at []Region) []Region {
	var out []Region
	for _, r := range at {
		if p.Permits(r) != nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// list renders regions for a message a person reads.
func list(rs []Region) string {
	names := make([]string, 0, len(rs))
	for _, r := range rs {
		names = append(names, string(r))
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
