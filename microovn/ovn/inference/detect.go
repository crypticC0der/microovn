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

// sysfsPCIDir is the sysfs location that exposes one directory per PCI device.
const sysfsPCIDir = "/sys/bus/pci/devices"

// PCIDevice is a single PCI device discovered on the host.
type PCIDevice struct {
	VendorID    uint16
	DeviceID    uint16
	DeviceClass uint16
}

// Hardware is a summary of the host hardware that is relevant to OVS engine
// selection.
type Hardware struct {
	Architecture string
	PCIDevices   []PCIDevice
}

// Detect enumerates the host hardware relevant to engine selection.
func Detect() (*Hardware, error) {
	return detect(os.DirFS(sysfsPCIDir), runtime.GOARCH)
}

// detect enumerates hardware from the given sysfs root and build architecture.
// The architecture is supplied by the caller so that the result reflects the
// platform the binaries were built for (which, for a snap, is the host).
func detect(fsys fs.FS, arch string) (*Hardware, error) {
	devices, err := scanPCI(fsys)
	if err != nil {
		return nil, err
	}

	return &Hardware{
		Architecture: arch,
		PCIDevices:   devices,
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
