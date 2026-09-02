//go:build unix

package topology

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// PoolStorage describes the filesystem a pool directory actually lives on.
//
// It exists because summing every local device answers the wrong question. The
// pools occupy one filesystem, and how much that filesystem holds is what
// decides how much a node can manage. A node with four NVMe drives and pools
// on one of them can lend what that one holds, not the sum.
type PoolStorage struct {
	// TotalBytes is the size of the filesystem holding the path.
	TotalBytes Fact[int64]
	// AvailableBytes is what is currently free on it, which is what compute
	// and the borrowed pool are actually competing for.
	AvailableBytes Fact[int64]
	// Device is the block device backing the filesystem, such as nvme0n1, and
	// is empty when it could not be determined.
	Device string
	// Local reports whether that device is attached to this machine, using the
	// same rules as any other device: a filesystem on a network volume is not
	// compute-local capacity however local the path looks.
	Local Fact[bool]
}

// DescribePool measures the filesystem holding path and identifies the device
// under it.
//
// root is the filesystem root used for device lookups, matching Discover, and
// mountinfo is where the mount table is read from, which is
// /proc/self/mountinfo in production and a fixture in tests.
func DescribePool(root, mountinfo, path string) PoolStorage {
	ps := PoolStorage{
		TotalBytes:     UnknownValue[int64](),
		AvailableBytes: UnknownValue[int64](),
		Local:          UnknownValue[bool](),
	}
	// An empty path means nobody configured this pool, and it must not be
	// answered. filepath.Abs turns "" into the working directory, so measuring
	// it returns a confident description of an unrelated filesystem, which a
	// caller then compares against a real pool and refuses over a mismatch
	// with a directory that was never configured at all.
	if path == "" {
		return ps
	}

	if st, ok := statfsNearest(path); ok && st.Bsize > 0 {
		block := int64(st.Bsize)
		ps.TotalBytes = DiscoveredValue(int64(st.Blocks) * block)
		ps.AvailableBytes = DiscoveredValue(int64(st.Bavail) * block)
	}

	ps.Device = deviceForPath(mountinfo, path)
	if ps.Device != "" {
		ps.Local = classifyLocality(root, ps.Device)
	}
	return ps
}

// statfsNearest measures the filesystem at path, walking up to the nearest
// ancestor that exists.
//
// A pool directory is routinely measured before it has been created, and
// creating it first is worse than walking: it writes to disk ahead of the
// checks that decide whether these directories are a legal pair at all. The
// ancestor is on the same filesystem unless the pool is itself a mount point,
// in which case it exists already and the walk does not run.
func statfsNearest(path string) (syscall.Statfs_t, bool) {
	dir, err := filepath.Abs(path)
	if err != nil {
		return syscall.Statfs_t{}, false
	}
	for {
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err == nil {
			return st, true
		} else if !os.IsNotExist(err) {
			return syscall.Statfs_t{}, false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return syscall.Statfs_t{}, false
		}
		dir = parent
	}
}

// OccupiedBytes sums what the given directories actually take up on disk.
//
// It is what Forebay already holds on a filesystem, and the point of knowing
// it is that this space is ours to hand out again: free space alone would
// under-report the ceiling by everything already lent.
//
// Allocated blocks are summed rather than file lengths, because an extent may
// be sparse and a hole costs nothing. A directory that cannot be walked yields
// unknown rather than a total that silently omits part of itself.
func OccupiedBytes(paths ...string) Fact[int64] {
	var total int64
	for _, root := range paths {
		if root == "" {
			continue
		}
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			switch {
			case os.IsNotExist(err):
				return whenVanished(p, root, d)
			case err != nil:
				return err
			case !d.Type().IsRegular():
				return nil
			}
			info, err := d.Info()
			if err != nil {
				if os.IsNotExist(err) {
					return nil // Vanished mid-walk; it holds nothing now.
				}
				return err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok {
				return fmt.Errorf("no block count for %s", p)
			}
			total += int64(st.Blocks) * 512
			return nil
		})
		if err != nil {
			return UnknownValue[int64]()
		}
	}
	return DiscoveredValue(total)
}

// whenVanished decides how far a walk should give up when an entry has gone
// away underneath it.
//
// Only a missing root ends the count, and it ends it at zero: a pool that was
// never written holds nothing, which is a measurement rather than a failure.
// Below the root, something disappearing is the normal operating condition,
// because reclaim unlinks extents continuously. Ending the walk there returned
// a partial sum as a complete one, which is the single thing this package
// exists to prevent.
func whenVanished(path, root string, d os.DirEntry) error {
	switch {
	case path == root:
		return filepath.SkipAll
	case d != nil && d.IsDir():
		// Skip the directory that went away, not its siblings.
		return filepath.SkipDir
	default:
		// A file that is gone occupies nothing; keep counting the rest.
		return nil
	}
}

// deviceForPath finds the block device backing a path by reading the mount
// table and taking the longest mount point that is a prefix of it.
//
// Longest wins because mounts nest: a path under a volume mounted inside
// another must be attributed to the inner one, and the shortest match would
// always be the root.
func deviceForPath(mountinfo, path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	f, err := os.Open(mountinfo)
	if err != nil {
		return ""
	}
	defer f.Close()

	best, bestSource := "", ""
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		mountPoint, source, ok := parseMountinfoLine(scan.Text())
		if !ok || !underMount(abs, mountPoint) {
			continue
		}
		if len(mountPoint) >= len(best) {
			best, bestSource = mountPoint, source
		}
	}
	return blockDeviceName(bestSource)
}

// parseMountinfoLine pulls the mount point and source device out of one line
// of /proc/self/mountinfo, whose fields are separated from the optional ones
// by a lone hyphen.
func parseMountinfoLine(line string) (mountPoint, source string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return "", "", false
	}
	mountPoint = fields[4]
	for i, f := range fields {
		if f == "-" && len(fields) > i+2 {
			return mountPoint, fields[i+2], true
		}
	}
	return "", "", false
}

// underMount reports whether path is at or below a mount point.
func underMount(path, mount string) bool {
	if mount == "/" {
		return true
	}
	return path == mount || strings.HasPrefix(path, mount+string(filepath.Separator))
}

// blockDeviceName turns a mount source such as /dev/nvme0n1p2 into the kernel
// device name the rest of this package uses, and returns empty for sources
// that are not block devices at all, such as overlay or tmpfs.
func blockDeviceName(source string) string {
	if !strings.HasPrefix(source, "/dev/") {
		return ""
	}
	name := strings.TrimPrefix(source, "/dev/")
	if name == "" || strings.Contains(name, "/") {
		// Something like /dev/mapper/vg-lv, which names a device-mapper target
		// rather than the device underneath it.
		return name[strings.LastIndex(name, "/")+1:]
	}
	return name
}

// String renders the pool storage for an operator, saying plainly which parts
// are unknown rather than printing a plausible number.
func (p PoolStorage) String() string {
	device := p.Device
	if device == "" {
		device = "unknown device"
	}
	local := "locality unknown"
	if v, ok := p.Local.Known(); ok {
		if v {
			local = "local"
		} else {
			local = "not local"
		}
	}
	total, ok := p.TotalBytes.Known()
	if !ok {
		return fmt.Sprintf("%s (%s), size unknown", device, local)
	}
	avail, _ := p.AvailableBytes.Known()
	return fmt.Sprintf("%s (%s), %d bytes total, %d available", device, local, total, avail)
}

// SameFilesystem reports whether two paths sit on the same filesystem.
//
// It compares the device the kernel reports for each, which is identity rather
// than a resemblance. Two matched NVMe drives in one node have the same size
// and the same layout, so anything that compares those cannot tell them apart,
// and a node with matched drives is the ordinary case rather than the exotic
// one.
//
// A path that cannot be reached is an error naming that path, not a no: a
// caller that cannot see a path has not learned it is a different filesystem,
// and one told only that something was unreachable goes looking at whichever
// of the two it happened to guess.
func SameFilesystem(a, b string) (bool, error) {
	var sa, sb syscall.Stat_t
	if err := syscall.Stat(a, &sa); err != nil {
		return false, fmt.Errorf("%s could not be reached: %w", a, err)
	}
	if err := syscall.Stat(b, &sb); err != nil {
		return false, fmt.Errorf("%s could not be reached: %w", b, err)
	}
	return sa.Dev == sb.Dev, nil
}
