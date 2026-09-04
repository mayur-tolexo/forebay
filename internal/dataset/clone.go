package dataset

import (
	"fmt"

	"github.com/mayur-tolexo/forebay/driver"
)

// CanClone reports whether a backend can make a clone that is one.
//
// Named after the capability rather than the operation. An operator reading
// that a backend does not declare clone knows what to change; one reading that
// cloning is not supported does not.
func CanClone(b *driver.Backend) error {
	if b == nil {
		return fmt.Errorf("dataset: no backend")
	}
	if b.Supports(driver.Clone) {
		return nil
	}
	// Copying the bytes is not offered as a fallback. The caller chose a clone
	// precisely to avoid the copy, and taking an hour to do what was
	// advertised as instant is worse than saying no.
	if b.Supports(driver.Snapshot) {
		return fmt.Errorf("dataset: this backend declares snapshot and not clone, and a clone that copies is not a clone")
	}
	return fmt.Errorf("dataset: this backend declares neither snapshot nor clone")
}
