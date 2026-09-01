package topology

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// acceleratorVendors are PCI vendor identifiers that make a display-class
// device a candidate accelerator. Vendor alone is not sufficient.
//
// The class alone is certainly not enough: a probe found an NVIDIA RTX 5090
// reporting class 0x030000 and a QEMU virtual display adapter reporting the
// identical class. But vendor alone is not enough either, and Intel is why.
// 0x8086 covers both a datacentre compute card and the integrated graphics on
// a great many server boards, and placing data next to an integrated display
// adapter is the failure this rule exists to prevent.
var acceleratorVendors = map[string]string{
	"0x10de": "NVIDIA",
	"0x1002": "AMD",
	"0x8086": "Intel",
}

// computeDrivers are kernel drivers that only bind to hardware which computes.
// A bound driver is the second half of the positive signal RFC-0003 requires:
// a vendor that makes accelerators, and a driver that only drives them.
var computeDrivers = map[string]bool{
	"nvidia": true, "nvidia_drm": true,
	"amdgpu": true,
	"xe":     true,
}

// displayClasses are the PCI classes an accelerator presents as. 0x0300 is a
// VGA-compatible controller, which is what an RTX 5090 reports, and 0x0302 is
// a 3D controller, which is what a headless datacentre card reports.
var displayClasses = []string{"0x0300", "0x0302"}

// Accelerator is a device that might compute, identified by more than its class.
type Accelerator struct {
	// PCIAddress locates the device, such as 0000:01:00.0.
	PCIAddress string
	// Vendor and Device are the raw identifiers, kept so a reader can check
	// the identification rather than trust it.
	Vendor, Device string
	// VendorName is the recognised vendor, and is why this was accepted as an
	// accelerator at all.
	VendorName string
	// NUMA is the node it is attached to, frequently unknown.
	NUMA Fact[int]
}

// Disk is a block device that could hold borrowed or donated capacity.
type Disk struct {
	// Name is the kernel name, such as nvme0n1.
	Name string
	// SizeBytes is the device's capacity.
	SizeBytes Fact[int64]
	// Rotational distinguishes spinning media, which is not what this project
	// is about but is worth knowing before lending it.
	Rotational Fact[bool]
	// Local reports whether the device is attached to this machine rather than
	// reached over a network.
	//
	// It matters more than it looks. A probe of a real node found an attached
	// Ceph RBD device alongside the NVMe, and counting it would have offered
	// somebody else's networked storage as compute-local capacity, which is
	// the one thing this project is not. Unknown is common, because an iSCSI
	// LUN presents as an ordinary SCSI disk and sysfs does not say otherwise.
	Local Fact[bool]
}

// Node is what one machine reports about itself, plus what an operator has
// said about where it sits.
type Node struct {
	// Rack can only be declared. Nothing on a machine knows which rack it is
	// in, so this is unknown unless somebody says otherwise.
	Rack Fact[string]
	// NUMANodes is how many NUMA domains the machine has.
	NUMANodes Fact[int]
	// Accelerators are devices identified by a positive signal, never by class
	// alone.
	Accelerators []Accelerator
	// Disks are block devices, excluding partitions and virtual devices.
	Disks []Disk
	// RDMA reports whether the kernel exposes an InfiniBand class holding a
	// device. Absent is a fact; unreadable is not.
	RDMA Fact[bool]
}

// Discover reads what the machine says about itself.
//
// root is the filesystem root to read from, which is "/" in production and a
// fixture directory in tests. Nothing here writes, and a probe from an
// unprivileged container with no host mounts read all of it, because a
// container is given /sys from the host already.
func Discover(root string) Node {
	return Node{
		Rack:         UnknownValue[string](),
		NUMANodes:    discoverNUMA(root),
		Accelerators: discoverAccelerators(root),
		Disks:        discoverDisks(root),
		RDMA:         discoverRDMA(root),
	}
}

// WithDeclaredRack returns a copy carrying an operator-supplied rack.
//
// Rack is the one fact the machine cannot supply, so it arrives this way or
// not at all. An empty string is treated as no declaration rather than as a
// rack named "", since a missing label and a blank one mean the same thing.
func (n Node) WithDeclaredRack(rack string) Node {
	if strings.TrimSpace(rack) == "" {
		return n
	}
	n.Rack = DeclaredValue(rack)
	return n
}

// SameRack reports whether two nodes are known to share a rack.
//
// An unknown rack answers no. Treating unknown as near would place data far
// away and call it close.
func SameRack(a, b Node) bool {
	ra, aok := a.Rack.Known()
	rb, bok := b.Rack.Known()
	return aok && bok && ra == rb
}

// DifferentRacks reports whether two nodes are known to be in different racks.
//
// An unknown rack also answers no. Treating unknown as separate would put two
// replicas in one failure domain while reporting them as spread.
//
// Both questions answering no is not a contradiction. An unknown never
// satisfies a requirement, whichever requirement is being asked.
func DifferentRacks(a, b Node) bool {
	ra, aok := a.Rack.Known()
	rb, bok := b.Rack.Known()
	return aok && bok && ra != rb
}

// discoverNUMA counts NUMA domains.
func discoverNUMA(root string) Fact[int] {
	entries, err := os.ReadDir(filepath.Join(root, "sys/devices/system/node"))
	if err != nil {
		return UnknownValue[int]()
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "node") {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimPrefix(name, "node")); err == nil {
			n++
		}
	}
	if n == 0 {
		return UnknownValue[int]()
	}
	return DiscoveredValue(n)
}

// discoverAccelerators finds display-class PCI devices from a vendor that
// makes compute hardware.
func discoverAccelerators(root string) []Accelerator {
	base := filepath.Join(root, "sys/bus/pci/devices")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []Accelerator
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		class := readTrimmed(filepath.Join(dir, "class"))
		if !hasAnyPrefix(class, displayClasses) {
			continue
		}
		vendor := readTrimmed(filepath.Join(dir, "vendor"))
		name, known := acceleratorVendors[vendor]
		if !known {
			// A display-class device from an unrecognised vendor is a display
			// adapter until something says otherwise. Guessing here moves data
			// towards a device that will never read it.
			continue
		}
		if !hasComputeDriver(dir) {
			// A candidate vendor with no compute driver bound is graphics.
			// Intel integrated display adapters share a vendor with Intel
			// datacentre accelerators, and only the driver separates them.
			continue
		}
		out = append(out, Accelerator{
			PCIAddress: e.Name(),
			Vendor:     vendor,
			Device:     readTrimmed(filepath.Join(dir, "device")),
			VendorName: name,
			NUMA:       readNUMA(filepath.Join(dir, "numa_node")),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PCIAddress < out[j].PCIAddress })
	return out
}

// hasComputeDriver reports whether a compute driver is bound to the device.
//
// The driver symlink names what the kernel actually attached, which is the
// difference between a card that computes and a display adapter from the same
// vendor. A device with no driver bound is not identified as an accelerator,
// because nothing has claimed it and guessing is how data ends up beside
// hardware that will never read it.
func hasComputeDriver(dir string) bool {
	target, err := os.Readlink(filepath.Join(dir, "driver"))
	if err != nil {
		return false
	}
	return computeDrivers[filepath.Base(target)]
}

// discoverDisks finds whole block devices, skipping partitions and virtual
// devices that hold nothing lendable.
func discoverDisks(root string) []Disk {
	base := filepath.Join(root, "sys/class/block")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []Disk
	for _, e := range entries {
		name := e.Name()
		if isSkippableBlockDevice(name) {
			continue
		}
		dir := filepath.Join(base, name)
		// A partition carries a partition file; the whole device does not.
		if _, err := os.Stat(filepath.Join(dir, "partition")); err == nil {
			continue
		}
		// A device built from other devices is skipped in favour of the
		// devices underneath it. Counting both would report twice the capacity
		// that exists, which on a node whose NVMe drives are in a RAID would
		// have the agent lending storage that is not there.
		if isStacked(dir) {
			continue
		}
		size := readSectors(filepath.Join(dir, "size"))
		// A device of known zero size holds nothing lendable. A real node
		// carried sixteen unused network block devices, all of them empty, and
		// listing them buries the disks that matter. A device of unknown size
		// is still reported, since unknown is not zero.
		if bytes, known := size.Known(); known && bytes == 0 {
			continue
		}
		out = append(out, Disk{
			Name:       name,
			SizeBytes:  size,
			Rotational: readBool(filepath.Join(dir, "queue/rotational")),
			Local:      classifyLocality(root, name),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// networkBlockPrefixes name devices that are definitely reached over a
// network. A real node carried sixteen unused nbd devices and an attached rbd.
var networkBlockPrefixes = []string{"nbd", "rbd", "drbd"}

// localNVMeTransports are the values /sys/class/nvme/nvmeN/transport takes for
// a device attached to this machine. Anything else, such as tcp, rdma or fc,
// is NVMe over fabrics and is a network device wearing a local name.
var localNVMeTransports = map[string]bool{"pcie": true}

// classifyLocality decides whether a device is attached to this machine.
//
// Only a positive signal makes it local, and only a positive signal makes it
// remote. Everything else is unknown, and an unknown is refused rather than
// lent.
//
// The name is not the signal, which is the trap here. An NVMe over fabrics
// namespace is called nvme0n1 exactly as a local drive is, and this project
// plans to use NVMe over fabrics, so trusting the prefix would have offered a
// network as local capacity. The kernel says which in the transport file. A
// virtio or Xen disk is local only from inside the guest and is routinely
// backed by network storage the guest cannot see, so it stays unknown. A SCSI
// disk may be a drive or an iSCSI LUN and sysfs does not distinguish them.
func classifyLocality(root, name string) Fact[bool] {
	for _, p := range networkBlockPrefixes {
		if strings.HasPrefix(name, p) {
			return DiscoveredValue(false)
		}
	}
	if strings.HasPrefix(name, "nvme") {
		return classifyNVMe(root, name)
	}
	return UnknownValue[bool]()
}

// classifyNVMe asks the kernel how an NVMe namespace is attached.
func classifyNVMe(root, name string) Fact[bool] {
	ctrl := nvmeController(name)
	if ctrl == "" {
		return UnknownValue[bool]()
	}
	transport := readTrimmed(filepath.Join(root, "sys/class/nvme", ctrl, "transport"))
	if transport == "" {
		return UnknownValue[bool]()
	}
	return DiscoveredValue(localNVMeTransports[transport])
}

// nvmeNamespace matches a namespace such as nvme0n1, or a partition of one
// such as nvme0n1p2, and captures the controller, nvme0.
//
// The partition suffix is not an afterthought. The filesystem holding the
// pools sits on a partition far more often than on a whole namespace, so a
// pattern that only matched whole devices classified real local NVMe as
// unknown and refused to lend it.
//
// Written as a pattern because scanning for the separator by hand finds the
// "n" that nvme itself starts with.
var nvmeNamespace = regexp.MustCompile(`^(nvme[0-9]+)n[0-9]+(?:p[0-9]+)?$`)

// nvmeController maps a namespace such as nvme0n1 to its controller, nvme0.
func nvmeController(namespace string) string {
	m := nvmeNamespace.FindStringSubmatch(namespace)
	if m == nil {
		return ""
	}
	return m[1]
}

// isStacked reports whether a block device is assembled from other block
// devices, such as a RAID array or a device-mapper target.
//
// The members are what physically hold bytes, so they are what gets counted.
// sysfs names them in the slaves directory, which is empty or absent for a
// device that is not built from anything.
func isStacked(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "slaves"))
	return err == nil && len(entries) > 0
}

// isSkippableBlockDevice reports whether a block device holds nothing worth
// lending, such as a loop mount or a device-mapper target.
func isSkippableBlockDevice(name string) bool {
	for _, p := range []string{"loop", "dm-", "ram", "zram", "sr"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// discoverRDMA reports whether the kernel exposes a usable InfiniBand device.
//
// A missing class directory means the kernel has no RDMA, which is a fact. A
// directory that cannot be read is unknown, and the two must not be confused:
// a fleet where detection failed is not a fleet without RDMA.
func discoverRDMA(root string) Fact[bool] {
	// A missing infiniband class means no RDMA only if sysfs is there to be
	// missing it from. Without sysfs the question was never asked, and
	// answering absent would report a fleet as having no RDMA when the truth
	// is that nothing could look.
	if _, err := os.Stat(filepath.Join(root, "sys/class")); err != nil {
		return UnknownValue[bool]()
	}
	entries, err := os.ReadDir(filepath.Join(root, "sys/class/infiniband"))
	if err != nil {
		if os.IsNotExist(err) {
			return DiscoveredValue(false)
		}
		return UnknownValue[bool]()
	}
	return DiscoveredValue(len(entries) > 0)
}

// readTrimmed reads a sysfs file, returning empty on any failure.
func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readNUMA reads a numa_node file, where -1 means not applicable rather than
// node minus one.
func readNUMA(path string) Fact[int] {
	s := readTrimmed(path)
	if s == "" {
		return UnknownValue[int]()
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return UnknownValue[int]()
	}
	return DiscoveredValue(n)
}

// readSectors reads a block device size, which sysfs reports in 512-byte
// sectors regardless of the device's own sector size.
func readSectors(path string) Fact[int64] {
	s := readTrimmed(path)
	if s == "" {
		return UnknownValue[int64]()
	}
	sectors, err := strconv.ParseInt(s, 10, 64)
	if err != nil || sectors < 0 {
		return UnknownValue[int64]()
	}
	return DiscoveredValue(sectors * 512)
}

// readBool reads a sysfs flag that is 0 or 1.
func readBool(path string) Fact[bool] {
	switch readTrimmed(path) {
	case "0":
		return DiscoveredValue(false)
	case "1":
		return DiscoveredValue(true)
	default:
		return UnknownValue[bool]()
	}
}

// hasAnyPrefix reports whether s starts with any of the prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
