package topology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures replicate two machines that were actually probed: a node with
// two NVIDIA RTX 5090s, and a virtual machine whose display adapter reports
// the identical PCI class. Testing against both is what proves the
// identification rule rather than merely exercising it.
const (
	gpuNode     = "testdata/gpu-node"
	virtualNode = "testdata/virtual-node"
)

func TestAcceleratorsNeedBothAVendorAndAComputeDriver(t *testing.T) {
	gpu := Discover(gpuNode)
	found := map[string]string{}
	for _, a := range gpu.Accelerators {
		found[a.PCIAddress] = a.VendorName
	}

	// Two NVIDIA cards with the nvidia driver bound, and one Intel datacentre
	// card with a compute driver bound.
	for addr, vendor := range map[string]string{
		"0000:01:00.0": "NVIDIA", "0000:04:00.0": "NVIDIA", "0000:06:00.0": "Intel",
	} {
		if found[addr] != vendor {
			t.Errorf("accelerator %s = %q, want %s", addr, found[addr], vendor)
		}
	}

	// An Intel integrated display adapter shares its vendor with the Intel
	// accelerator above and differs only in the driver bound to it. Vendor
	// alone would place data next to a device that cannot compute.
	if _, ok := found["0000:00:02.0"]; ok {
		t.Error("the Intel integrated display adapter was identified as an accelerator")
	}
	if len(gpu.Accelerators) != 3 {
		t.Errorf("found %d accelerators, want 3: %+v", len(gpu.Accelerators), gpu.Accelerators)
	}

	// The virtual node's adapter reports class 0x030000, exactly as the RTX
	// 5090 does. Only the vendor separates them, so a class match alone would
	// place data next to a device that cannot compute.
	virt := Discover(virtualNode)
	if len(virt.Accelerators) != 0 {
		t.Errorf("found %d accelerators on the virtual node, want none: %+v",
			len(virt.Accelerators), virt.Accelerators)
	}
}

func TestNUMAMinusOneIsUnknownRatherThanZero(t *testing.T) {
	// Every PCI device on the probed hardware reported -1, which means not
	// applicable. Read as a number it would place every device as though it
	// shared one NUMA node.
	for _, a := range Discover(gpuNode).Accelerators {
		if a.PCIAddress == "0000:06:00.0" {
			// This one reports a real affinity, so the model must carry it.
			if v, ok := a.NUMA.Known(); !ok || v != 1 {
				t.Errorf("accelerator %s NUMA = %d/%v, want a known 1", a.PCIAddress, v, ok)
			}
			continue
		}
		if v, ok := a.NUMA.Known(); ok {
			t.Errorf("accelerator %s reported NUMA %d as known, want unknown", a.PCIAddress, v)
		}
	}
}

// disk finds a named device in a discovered node.
func disk(t *testing.T, n Node, name string) Disk {
	t.Helper()
	for _, d := range n.Disks {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no disk named %s in %+v", name, n.Disks)
	return Disk{}
}

// knownLocalDisks names the disks a node may lend from: known to be local, and
// known to have a size. Both unknowns are excluded rather than guessed, since
// lending against a guess is the failure this package exists to prevent.
func knownLocalDisks(n Node) []string {
	var out []string
	for _, d := range n.Disks {
		if local, ok := d.Local.Known(); !ok || !local {
			continue
		}
		if _, ok := d.SizeBytes.Known(); ok {
			out = append(out, d.Name)
		}
	}
	return out
}

func TestDisksExcludePartitionsAndVirtualDevices(t *testing.T) {
	n := Discover(gpuNode)
	for _, unwanted := range []string{"nvme0n1p1", "loop0"} {
		for _, d := range n.Disks {
			if d.Name == unwanted {
				t.Errorf("%s was reported as a disk, want partitions and loop devices skipped", unwanted)
			}
		}
	}
	size, ok := disk(t, n, "nvme0n1").SizeBytes.Known()
	if !ok {
		t.Fatal("NVMe size is unknown, want it discovered")
	}
	// sysfs reports 512-byte sectors regardless of the device's sector size.
	if want := int64(4000797360) * 512; size != want {
		t.Errorf("size = %d, want %d", size, want)
	}
	if rot, ok := disk(t, n, "nvme0n1").Rotational.Known(); !ok || rot {
		t.Errorf("rotational = %v/%v, want a known false", rot, ok)
	}
	if got := knownLocalDisks(n); len(got) != 1 || got[0] != "nvme0n1" {
		t.Errorf("lendable disks = %v, want only the local NVMe", got)
	}
}

func TestNetworkDevicesAreNotCountedAsLocalCapacity(t *testing.T) {
	// A real node carried an attached Ceph RBD alongside its NVMe. Counting it
	// would offer somebody else's networked storage as compute-local capacity,
	// which is the one thing this project is not.
	n := Discover(gpuNode)

	rbd := disk(t, n, "rbd0")
	if local, ok := rbd.Local.Known(); !ok || local {
		t.Errorf("rbd0 locality = %v/%v, want a known false", local, ok)
	}
	if _, ok := rbd.SizeBytes.Known(); !ok {
		t.Error("rbd0 size is unknown, want it reported even though it is not counted")
	}

	// A SCSI disk may be a local drive or an iSCSI LUN, and sysfs does not say,
	// so it is unknown and therefore uncounted rather than assumed local.
	if _, ok := disk(t, n, "sda").Local.Known(); ok {
		t.Error("sda locality was answered, want unknown since it could be an iSCSI LUN")
	}

	// The trap the name hides: an NVMe over fabrics namespace is called
	// nvme1n1 exactly as a local drive is, and this project plans to use NVMe
	// over fabrics, so the kernel's transport is the only honest signal.
	fabrics := disk(t, n, "nvme1n1")
	if local, ok := fabrics.Local.Known(); !ok || local {
		t.Errorf("nvme1n1 over tcp locality = %v/%v, want a known false", local, ok)
	}

	if got := knownLocalDisks(n); len(got) != 1 || got[0] != "nvme0n1" {
		t.Errorf("lendable disks = %v, want only the device known to be local", got)
	}
}

func TestRDMAAbsenceIsAFactAndUnreadableIsNot(t *testing.T) {
	// Neither fixture has an infiniband class, which is a definitive absence.
	rdma, ok := Discover(gpuNode).RDMA.Known()
	if !ok {
		t.Fatal("RDMA is unknown, want a known absence")
	}
	if rdma {
		t.Error("RDMA reported present, want absent")
	}

	// A root with no /sys at all cannot answer, which is different.
	missing := Discover(filepath.Join("testdata", "no-such-root"))
	if _, ok := missing.RDMA.Known(); ok {
		t.Error("RDMA answered from a root with no sysfs, want unknown")
	}
	if _, ok := missing.NUMANodes.Known(); ok {
		t.Error("NUMA answered from a root with no sysfs, want unknown")
	}
}

func TestAnUnknownNeverSatisfiesAnyRequirement(t *testing.T) {
	// The rule the whole model rests on. Both questions answer no, which is
	// not a contradiction: an unknown cannot be used to claim closeness and
	// cannot be used to claim separation.
	unknown, other := Discover(gpuNode), Discover(virtualNode)

	if SameRack(unknown, other) {
		t.Error("SameRack said yes with no rack known, want no")
	}
	if DifferentRacks(unknown, other) {
		t.Error("DifferentRacks said yes with no rack known, want no")
	}

	a := unknown.WithDeclaredRack("r1")
	if SameRack(a, other) || DifferentRacks(a, other) {
		t.Error("one known rack was enough to answer, want both no")
	}

	b := other.WithDeclaredRack("r1")
	if !SameRack(a, b) {
		t.Error("SameRack said no for two nodes declared in r1")
	}
	if DifferentRacks(a, b) {
		t.Error("DifferentRacks said yes for two nodes declared in r1")
	}

	c := other.WithDeclaredRack("r2")
	if SameRack(a, c) {
		t.Error("SameRack said yes for r1 and r2")
	}
	if !DifferentRacks(a, c) {
		t.Error("DifferentRacks said no for r1 and r2")
	}
}

func TestADeclaredRackIsDeclaredAndAnEmptyOneIsNot(t *testing.T) {
	n := Discover(gpuNode)
	if got := n.Rack.Provenance(); got != Unknown {
		t.Errorf("a discovered node had rack provenance %s, want unknown: nothing on a machine knows its rack", got)
	}
	if got := n.WithDeclaredRack("rack-7").Rack.Provenance(); got != Declared {
		t.Errorf("declared rack provenance = %s, want declared", got)
	}
	// A blank label is a missing label, not a rack named "".
	for _, blank := range []string{"", "   "} {
		if _, ok := n.WithDeclaredRack(blank).Rack.Known(); ok {
			t.Errorf("a blank rack label %q was accepted as a declaration", blank)
		}
	}
}

func TestFactOrIsForDisplayAndKeepsProvenance(t *testing.T) {
	known := DiscoveredValue(7)
	if got := known.Or(99); got != 7 {
		t.Errorf("Or on a known fact = %d, want 7", got)
	}
	unknown := UnknownValue[int]()
	if got := unknown.Or(99); got != 99 {
		t.Errorf("Or on an unknown fact = %d, want the fallback", got)
	}
	if got := unknown.Provenance(); got != Unknown {
		t.Errorf("Or changed provenance to %s, want it untouched", got)
	}
}

func TestProvenanceNames(t *testing.T) {
	for p, want := range map[Provenance]string{
		Unknown: "unknown", Discovered: "discovered", Declared: "declared", Provenance(9): "unknown",
	} {
		if got := p.String(); got != want {
			t.Errorf("Provenance(%d).String() = %q, want %q", p, got, want)
		}
	}
}

func TestEmptyDevicesAreNotReportedButUnknownSizesAre(t *testing.T) {
	// A real node carried sixteen unused network block devices, all empty.
	// Listing them buries the disks that matter, while a device whose size
	// could not be read is a different thing and stays visible.
	n := Discover(gpuNode)
	for _, d := range n.Disks {
		if size, known := d.SizeBytes.Known(); known && size == 0 {
			t.Errorf("empty device %s was reported, want it skipped", d.Name)
		}
	}
	for _, d := range n.Disks {
		if d.Name == "nbd0" {
			t.Error("nbd0 has a known size of zero and was reported")
		}
	}
}

func TestLocalNVMeIsIdentifiedByTransportNotName(t *testing.T) {
	n := Discover(gpuNode)
	local := disk(t, n, "nvme0n1")
	if got, ok := local.Local.Known(); !ok || !got {
		t.Errorf("nvme0n1 over pcie = %v/%v, want a known true", got, ok)
	}
	// Only the pcie device is lendable, even though a fabrics namespace of
	// similar size sits beside it wearing an almost identical name.
	if got := knownLocalDisks(n); len(got) != 1 || got[0] != "nvme0n1" {
		t.Errorf("lendable disks = %v, want only the pcie device", got)
	}
}

func TestAVirtioDiskIsUnknownRatherThanLocal(t *testing.T) {
	// Local from inside the guest, and routinely backed by network storage the
	// guest cannot see.
	if got := classifyLocality("testdata/gpu-node", "vda"); got.Provenance() != Unknown {
		t.Errorf("vda locality = %s, want unknown", got.Provenance())
	}
	if got := classifyLocality("testdata/gpu-node", "xvdb"); got.Provenance() != Unknown {
		t.Errorf("xvdb locality = %s, want unknown", got.Provenance())
	}
}

func TestNVMeControllerParsing(t *testing.T) {
	// The obvious hand-written version scans for "n" and finds the one that
	// nvme itself begins with, which silently made every NVMe device unknown.
	for in, want := range map[string]string{
		"nvme0n1": "nvme0", "nvme12n3": "nvme12",
		// A partition of a namespace resolves to the same controller. The
		// filesystem holding the pools is usually on one of these, and a
		// pattern that missed them refused real local NVMe as unknown.
		"nvme0n1p2": "nvme0", "nvme3n1p11": "nvme3",
		"nvme0": "", "sda": "", "sda1": "", "": "",
	} {
		if got := nvmeController(in); got != want {
			t.Errorf("nvmeController(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestARaidAndItsMembersAreNotBothCounted(t *testing.T) {
	// A GPU node with its NVMe drives in a RAID would otherwise report twice
	// the capacity that exists, and the agent would lend storage that is not
	// there. The members hold the bytes, so the members are what count.
	n := Discover(gpuNode)
	for _, d := range n.Disks {
		if d.Name == "md0" {
			t.Error("md0 was reported as a disk, want the devices underneath it counted instead")
		}
	}
	if got := knownLocalDisks(n); len(got) != 1 || got[0] != "nvme0n1" {
		t.Errorf("lendable disks = %v, want the member alone, never the array as well", got)
	}
}

func TestPoolStorageIsMeasuredOnTheFilesystemHoldingIt(t *testing.T) {
	// Summing every local device answers the wrong question. The pools live on
	// one filesystem, and how much that one holds is what bounds the node.
	dir := t.TempDir()
	ps := DescribePool(gpuNode, "testdata/mountinfo", dir)

	total, ok := ps.TotalBytes.Known()
	if !ok || total <= 0 {
		t.Fatalf("TotalBytes = %d/%v, want a positive size from statfs", total, ok)
	}
	avail, ok := ps.AvailableBytes.Known()
	if !ok || avail < 0 || avail > total {
		t.Errorf("AvailableBytes = %d/%v, want a sane value under %d", avail, ok, total)
	}
}

func TestTheDeviceUnderAPathIsFoundByLongestMount(t *testing.T) {
	// Mounts nest, so the shortest match is always the root and would
	// attribute every path to it.
	for path, want := range map[string]string{
		"/var/lib/forebay": "nvme0n1p2",
		"/boot/efi":        "nvme0n1p1",
		"/mnt/rbd/pools":   "rbd0",
		"/mnt/remote/x":    "", // a ceph mount names no block device
		"/overlay/upper":   "", // nor does an overlay
	} {
		if got := deviceForPath("testdata/mountinfo", path); got != want {
			t.Errorf("deviceForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestAPoolOnANetworkVolumeIsNotLocal(t *testing.T) {
	// A filesystem on an RBD is not compute-local capacity however local the
	// path looks from inside the container.
	ps := DescribePool(gpuNode, "testdata/mountinfo", "/mnt/rbd/pools")
	if ps.Device != "rbd0" {
		t.Fatalf("device = %q, want rbd0", ps.Device)
	}
	if local, ok := ps.Local.Known(); !ok || local {
		t.Errorf("locality = %v/%v, want a known false", local, ok)
	}
}

func TestBlockDeviceNaming(t *testing.T) {
	for in, want := range map[string]string{
		"/dev/nvme0n1":      "nvme0n1",
		"/dev/mapper/vg-lv": "vg-lv",
		"overlay":           "",
		"tmpfs":             "",
		"":                  "",
	} {
		if got := blockDeviceName(in); got != want {
			t.Errorf("blockDeviceName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPoolStorageSaysWhatItDoesNotKnow(t *testing.T) {
	// This string is what an operator reads when a node refuses to lend, so
	// each unknown has to be visible as an unknown rather than as a plausible
	// number or a confident "not local".
	for _, c := range []struct {
		name string
		ps   PoolStorage
		want string
	}{
		{"everything known", PoolStorage{
			TotalBytes: DiscoveredValue[int64](100), AvailableBytes: DiscoveredValue[int64](40),
			Device: "nvme0n1p2", Local: DiscoveredValue(true),
		}, "nvme0n1p2 (local), 100 bytes total, 40 available"},
		{"known not local", PoolStorage{
			TotalBytes: DiscoveredValue[int64](100), AvailableBytes: DiscoveredValue[int64](40),
			Device: "rbd0", Local: DiscoveredValue(false),
		}, "rbd0 (not local), 100 bytes total, 40 available"},
		{"locality unknown", PoolStorage{
			TotalBytes: DiscoveredValue[int64](100), AvailableBytes: DiscoveredValue[int64](40),
			Device: "sda1", Local: UnknownValue[bool](),
		}, "sda1 (locality unknown), 100 bytes total, 40 available"},
		{"no device at all", PoolStorage{
			TotalBytes: UnknownValue[int64](), AvailableBytes: UnknownValue[int64](),
			Local: UnknownValue[bool](),
		}, "unknown device (locality unknown), size unknown"},
	} {
		if got := c.ps.String(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMalformedMountinfoLinesAreSkipped(t *testing.T) {
	// A line without the lone hyphen that separates the optional fields has no
	// source device to read, and a short line has no mount point. Guessing at
	// either would attribute the pools to a device chosen at random.
	for _, line := range []string{
		"", "27 26 0:23 / /run", "31 27 8:1 / /boot rw,relatime no hyphen here",
	} {
		if _, _, ok := parseMountinfoLine(line); ok {
			t.Errorf("parsed a line that should have been skipped: %q", line)
		}
	}
}

func TestFixturesSurviveAClone(t *testing.T) {
	// Git does not record empty directories, so a fixture that relies on one
	// exists on the machine that made it and nowhere else. That is not a
	// cosmetic problem: the md RAID fixture proved a device with members is
	// skipped, and with its slaves directory gone on a fresh clone the test
	// asserted the opposite of what it was written to assert, green locally
	// and wrong in CI. Sysfs directories that matter here always hold
	// something, so an empty one is a fixture that will not survive.
	err := filepath.WalkDir("testdata", func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			t.Errorf("%s is empty, so git will not track it and CI will not have it", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking testdata: %v", err)
	}
}

func TestAPoolIsMeasuredBeforeItExists(t *testing.T) {
	// Creating the directory in order to measure it writes to disk ahead of
	// the checks that decide whether these directories are a legal pair at
	// all, so the measurement walks up to the nearest ancestor instead.
	root := t.TempDir()
	deep := filepath.Join(root, "not", "created", "yet")

	ps := DescribePool("/", "/proc/self/mountinfo", deep)
	total, ok := ps.TotalBytes.Known()
	if !ok || total <= 0 {
		t.Fatalf("TotalBytes = %d/%v for a path that does not exist yet, want the ancestor's filesystem", total, ok)
	}
	if _, err := os.Stat(filepath.Join(root, "not")); err == nil {
		t.Error("measuring created the directory, want nothing written")
	}

	// The ancestor is the same filesystem, so the answer must not change once
	// the directory is really there.
	if err := os.MkdirAll(deep, 0o750); err != nil {
		t.Fatal(err)
	}
	after, _ := DescribePool("/", "/proc/self/mountinfo", deep).TotalBytes.Known()
	if after != total {
		t.Errorf("TotalBytes = %d after creating the directory, want the same %d", after, total)
	}
}

func TestOccupiedBytesCountsWhatIsOnDiskNotWhatIsClaimed(t *testing.T) {
	root := t.TempDir()
	if got, ok := OccupiedBytes(root).Known(); !ok || got != 0 {
		t.Errorf("OccupiedBytes of an empty pool = %d/%v, want a known 0", got, ok)
	}

	const size = 1 << 20
	if err := os.WriteFile(filepath.Join(root, "extent"), make([]byte, size), 0o640); err != nil {
		t.Fatal(err)
	}
	got, ok := OccupiedBytes(root).Known()
	if !ok {
		t.Fatal("OccupiedBytes is unknown, want it measured")
	}
	// Allocated blocks, so the answer is about a megabyte but need not be
	// exactly one: the filesystem decides how many blocks that costs.
	if got < size/2 || got > 4*size {
		t.Errorf("OccupiedBytes = %d, want roughly %d", got, size)
	}

	// A pool that was never created holds nothing, which is not a measurement
	// failure. A pool that cannot be walked is unknown, and the two must not
	// be confused.
	if v, ok := OccupiedBytes(filepath.Join(root, "never-made")).Known(); !ok || v != 0 {
		t.Errorf("OccupiedBytes of an absent pool = %d/%v, want a known 0", v, ok)
	}
	if _, ok := OccupiedBytes("").Known(); !ok {
		t.Error("OccupiedBytes of no pools at all is unknown, want a known 0")
	}
}

func TestOccupiedBytesSumsEveryPool(t *testing.T) {
	// The agent hands it both pools at once, and a total that silently omitted
	// one would leave that space reserved for others forever.
	a, b := t.TempDir(), t.TempDir()
	const size = 1 << 20
	for _, dir := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(dir, "extent"), make([]byte, size), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	one, _ := OccupiedBytes(a).Known()
	both, _ := OccupiedBytes(a, b).Known()
	if both <= one {
		t.Errorf("OccupiedBytes(a, b) = %d, want more than OccupiedBytes(a) = %d", both, one)
	}
}

func TestAnUnconfiguredPoolIsNotTheWorkingDirectory(t *testing.T) {
	// filepath.Abs turns "" into the working directory, so an unconfigured
	// pool came back as a confident description of whatever filesystem the
	// binary happened to be started from. A caller then compared that against
	// a real pool and refused over a mismatch with a directory nobody had
	// configured, hiding the actual "pool directories must be configured".
	ps := DescribePool("/", "/proc/self/mountinfo", "")
	if _, ok := ps.TotalBytes.Known(); ok {
		t.Errorf("an empty path was measured: %s", ps)
	}
	if _, ok := ps.AvailableBytes.Known(); ok {
		t.Errorf("an empty path reported free space: %s", ps)
	}
	if ps.Device != "" {
		t.Errorf("an empty path named device %q, want none", ps.Device)
	}
	if _, ok := ps.Local.Known(); ok {
		t.Errorf("an empty path answered locality: %s", ps)
	}
}

// fakeEntry is a directory entry that is not on disk, so the decision made
// when something vanishes mid-walk can be tested without racing a deletion.
type fakeEntry struct {
	os.DirEntry
	dir bool
}

func (f fakeEntry) IsDir() bool { return f.dir }

func TestAVanishedEntryOnlyEndsTheCountAtTheRoot(t *testing.T) {
	// Reclaim unlinks extents continuously, so an entry disappearing partway
	// through a walk is the normal operating condition. Giving up on the whole
	// walk there returned a partial sum as a complete one, which is an unknown
	// dressed as a fact.
	const root = "/pools/borrowed"
	for _, c := range []struct {
		name string
		path string
		d    os.DirEntry
		want error
	}{
		{"the pool itself was never written", root, nil, filepath.SkipAll},
		{"a directory under it went away", root + "/sub", fakeEntry{dir: true}, filepath.SkipDir},
		{"an extent under it went away", root + "/sub/extent", fakeEntry{dir: false}, nil},
		{"an entry with no type information", root + "/extent", nil, nil},
	} {
		if got := whenVanished(c.path, root, c.d); got != c.want {
			t.Errorf("%s: whenVanished = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAPoolThatWasNeverWrittenHoldsNothing(t *testing.T) {
	// The root case end to end: absent is a measurement of zero, never a
	// failure to measure, because the agent asks before creating the pools.
	got, ok := OccupiedBytes(filepath.Join(t.TempDir(), "never-made")).Known()
	if !ok || got != 0 {
		t.Errorf("OccupiedBytes of an absent pool = %d/%v, want a known 0", got, ok)
	}
}

func TestSameFilesystemComparesDeviceNotSize(t *testing.T) {
	// Two directories on one filesystem are the same filesystem however
	// different they look, which is what a size comparison gets wrong on a
	// node with two matched drives.
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	same, err := SameFilesystem(a, b)
	if err != nil || !same {
		t.Errorf("SameFilesystem(%s, %s) = %v, %v; want true and no error", a, b, same, err)
	}
}

func TestAPathThatCannotBeReachedIsNotAnAnswer(t *testing.T) {
	// Not being able to see a path is a different thing from having learned
	// it is a different filesystem, and a caller that conflates them refuses
	// a node that is fine.
	absent := filepath.Join(t.TempDir(), "absent")
	_, err := SameFilesystem(absent, t.TempDir())
	if err == nil {
		t.Fatal("an unreachable path was reported as a known answer")
	}
	// Which path, not merely that one of them failed: an operator told only
	// that something was unreachable checks whichever they guess.
	if !strings.Contains(err.Error(), absent) {
		t.Errorf("the error does not name the path it could not reach: %v", err)
	}

	_, err = SameFilesystem(t.TempDir(), absent)
	if err == nil || !strings.Contains(err.Error(), absent) {
		t.Errorf("the second path being unreachable is not named: %v", err)
	}
}
