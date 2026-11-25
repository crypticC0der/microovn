package inference

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
)

// Sysfs and procfs locations that expose the hardware relevant to engine
// selection.
const (
	sysfsPCIDir       = "/sys/bus/pci/devices"
	sysfsHugepagesDir = "/sys/kernel/mm/hugepages"
	sysfsIOMMUDir     = "/sys/class/iommu"
	procModulesFile   = "modules"
)

// PCIDevice is a single PCI device discovered on the host.
type PCIDevice struct {
	VendorID    uint16
	DeviceID    uint16
	DeviceClass uint16
}

// Hugepage describes a single hugepage pool: its page size and the number of
// pages reserved at boot.
type Hugepage struct {
	SizeKiB  uint64
	Reserved uint64
}

// Hardware is a summary of the host hardware that is relevant to OVS engine
// selection.
type Hardware struct {
	Architecture  string
	PCIDevices    []PCIDevice
	KernelModules []string
	Hugepages     []Hugepage
	IOMMUEnabled  bool
}

// detectionFilesystems groups the host filesystems the detectors read from
// under a common name, so tests can substitute a fake filesystem.
type detectionFilesystems struct {
	PCI      fs.FS
	Proc     fs.FS
	Hugepage fs.FS
	IOMMU    fs.FS
}

// Detect enumerates the host hardware relevant to engine selection.
func Detect() (*Hardware, error) {
	return detect(detectionFilesystems{
		PCI:      os.DirFS(sysfsPCIDir),
		Proc:     os.DirFS("/proc"),
		Hugepage: os.DirFS(sysfsHugepagesDir),
		IOMMU:    os.DirFS(sysfsIOMMUDir),
	}, runtime.GOARCH)
}

// detect enumerates hardware from the given filesystems and build architecture.
// The architecture is supplied by the caller so that the result reflects the
// platform the binaries were built for (which, for a snap, is the host).
//
// Each source is probed best-effort: an unreadable source yields an empty
// result rather than failing the whole scan, so a host that cannot be fully
// inspected conservatively fails readiness checks instead of crashing.
func detect(fsys detectionFilesystems, arch string) (*Hardware, error) {
	devices, err := scanPCI(fsys.PCI)
	if err != nil {
		return nil, err
	}

	modules := scanKernelModules(fsys.Proc)
	hugepages := scanHugepages(fsys.Hugepage)

	return &Hardware{
		Architecture:  arch,
		PCIDevices:    devices,
		KernelModules: modules,
		Hugepages:     hugepages,
		IOMMUEnabled:  iommuEnabled(fsys.IOMMU),
	}, nil
}

// scanPCI reads the PCI devices exposed under the given sysfs root. Devices
// whose attributes cannot be read are skipped rather than failing the whole
// scan, since a confined process may only be granted access to a subset.
func scanPCI(fsys fs.FS) ([]PCIDevice, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("reading pci devices: %w", err)
	}

	devices := make([]PCIDevice, 0, len(entries))

	for _, entry := range entries {
		device, err := readPCIDevice(fsys, entry.Name())
		if err != nil {
			continue
		}

		devices = append(devices, device)
	}

	return devices, nil
}

// readPCIDevice reads vendor, device and class attributes of a single PCI
// device directory.
func readPCIDevice(fsys fs.FS, name string) (PCIDevice, error) {
	vendor, err := readHexAttribute(fsys, path.Join(name, "vendor"))
	if err != nil {
		return PCIDevice{}, err
	}

	device, err := readHexAttribute(fsys, path.Join(name, "device"))
	if err != nil {
		return PCIDevice{}, err
	}

	class, err := readHexAttribute(fsys, path.Join(name, "class"))
	if err != nil {
		return PCIDevice{}, err
	}

	return PCIDevice{
		VendorID:    uint16(vendor),
		DeviceID:    uint16(device),
		DeviceClass: uint16(class >> 8),
	}, nil
}

// scanKernelModules returns the names of the currently loaded kernel modules
// by reading /proc/modules. An unreadable file yields an empty result.
func scanKernelModules(fsys fs.FS) []string {
	data, err := fs.ReadFile(fsys, procModulesFile)
	if err != nil {
		return nil
	}

	modules := make([]string, 0)

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			modules = append(modules, fields[0])
		}
	}

	return modules
}

// scanHugepages returns every hugepage pool that has at least one page
// reserved, reading the /sys/kernel/mm/hugepages directory. Each pool records
// its page size and the number of pages reserved at boot. Hugepage sizes with
// no pages reserved are omitted, as are directories whose contents cannot be
// read.
func scanHugepages(fsys fs.FS) []Hugepage {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil
	}

	pools := make([]Hugepage, 0)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		sizeKiB, ok := parseHugepageDirName(entry.Name())
		if !ok {
			continue
		}

		data, err := fs.ReadFile(fsys, path.Join(entry.Name(), "nr_hugepages"))
		if err != nil {
			continue
		}

		reserved, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil || reserved == 0 {
			continue
		}

		pools = append(pools, Hugepage{SizeKiB: sizeKiB, Reserved: reserved})
	}

	return pools
}

// parseHugepageDirName extracts the size in KiB from a hugepage directory name
// such as "hugepages-1048576kB".
func parseHugepageDirName(name string) (uint64, bool) {
	rest, ok := strings.CutPrefix(name, "hugepages-")
	if !ok {
		return 0, false
	}

	size, ok := strings.CutSuffix(rest, "kB")
	if !ok {
		return 0, false
	}

	parsed, err := strconv.ParseUint(size, 10, 64)
	if err != nil {
		return 0, false
	}

	return parsed, true
}

// iommuEnabled reports whether an IOMMU is present and enabled, detected by a
// non-empty /sys/class/iommu directory.
func iommuEnabled(fsys fs.FS) bool {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return false
	}

	return len(entries) > 0
}

// readHexAttribute reads a sysfs file containing a hexadecimal value such as
// "0x8086".
func readHexAttribute(fsys fs.FS, name string) (uint32, error) {
	file, err := fsys.Open(name)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, 32))
	if err != nil {
		return 0, err
	}

	value := strings.TrimPrefix(strings.TrimSpace(string(data)), "0x")
	parsed, err := strconv.ParseUint(value, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing attribute %q: %w", name, err)
	}

	return uint32(parsed), nil
}
