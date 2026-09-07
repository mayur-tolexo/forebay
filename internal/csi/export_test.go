package csi

// InMountinfo exposes the mount-table reader to this package's tests, so the
// parsing can be exercised against a table from a real node without one.
var InMountinfo = inMountinfo
