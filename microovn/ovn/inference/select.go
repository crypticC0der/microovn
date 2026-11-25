package inference

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNoCompatibleEngine is returned by Select when no engine matches the host
// hardware.
var ErrNoCompatibleEngine = errors.New("no compatible engine found")

// Match is the result of evaluating a single engine against the host hardware.
type Match struct {
	Engine     Engine
	Score      int
	Compatible bool
	// Missing holds a human-readable description of each requirement that
	// was not satisfied. It is empty when the engine is compatible.
	Missing []string
}

// Score evaluates an engine against the host hardware.
//
// It mirrors the filtering and ranking steps of inference snap engine
// selection: mandatory requirements (Allof) must all be satisfied and
// alternative requirements (Anyof) must have at least one match. The score
// reflects how specifically the requirements match the host, so that engines
// targeting more specific hardware rank above generic ones. Whenever a
// requirement is not met the reason is recorded in Missing so the caller can
// report exactly which prerequisites a host lacks.
func Score(engine Engine, hardware *Hardware) Match {
	match := Match{Engine: engine, Compatible: true}

	mandatory := len(engine.Devices.Allof)
	alternatives := len(engine.Devices.Anyof)

	if mandatory == 0 && alternatives == 0 {
		match.Score = 1
		return match
	}

	for _, device := range engine.Devices.Allof {
		score, ok := matchDevice(device, hardware)
		if !ok {
			match.Compatible = false
			match.Missing = append(match.Missing, deviceLabel(device))
			continue
		}

		match.Score += score
	}

	if match.Compatible && alternatives > 0 {
		found := false

		for _, device := range engine.Devices.Anyof {
			score, ok := matchDevice(device, hardware)
			if !ok {
				continue
			}

			match.Score += score
			found = true
		}

		if !found {
			match.Compatible = false

			for _, device := range engine.Devices.Anyof {
				match.Missing = append(match.Missing, deviceLabel(device))
			}
		}
	}

	if !match.Compatible {
		match.Score = 0
	}

	return match
}

// Select returns the highest scoring compatible engine for the host hardware.
// Engines marked as "devel" are excluded from automatic selection.
func Select(engines []Engine, hardware *Hardware) (*Engine, error) {
	var best *Match

	for i := range engines {
		if engines[i].Grade == "devel" {
			continue
		}

		match := Score(engines[i], hardware)
		if !match.Compatible || match.Score <= 0 {
			continue
		}

		if best == nil || match.Score > best.Score {
			selected := match
			best = &selected
		}
	}

	if best == nil {
		return nil, ErrNoCompatibleEngine
	}

	return &best.Engine, nil
}

// matchDevice reports whether a single device requirement is satisfied by the
// host hardware and, if so, how specifically it matched.
func matchDevice(device Device, hardware *Hardware) (int, bool) {
	switch device.Type {
	case "cpu":
		if device.Architecture == "" || device.Architecture != hardware.Architecture {
			return 0, false
		}

		return 1, true
	case "pci", "":
		return matchPCI(device, hardware.PCIDevices)
	case "kernel-module":
		return matchKernelModule(device, hardware.KernelModules)
	case "hugepages":
		return matchHugepages(device, hardware.Hugepages)
	case "iommu":
		if hardware.IOMMUEnabled {
			return 1, true
		}

		return 0, false
	default:
		return 0, false
	}
}

// matchKernelModule reports whether the named kernel module is loaded.
func matchKernelModule(device Device, modules []string) (int, bool) {
	if device.Name == "" {
		return 0, false
	}

	for _, module := range modules {
		if module == device.Name {
			return 2, true
		}
	}

	return 0, false
}

// matchHugepages reports whether the host has a hugepage pool of the requested
// size with at least the requested number of pages reserved. A minimum page
// count may be omitted, in which case any reserved pool of the right size
// matches.
func matchHugepages(device Device, pools []Hugepage) (int, bool) {
	sizeKiB, ok := parseSize(device.Size)
	if !ok {
		return 0, false
	}

	for _, pool := range pools {
		if pool.SizeKiB == sizeKiB && pool.Reserved >= device.Num {
			return 2, true
		}
	}

	return 0, false
}

// deviceLabel returns a short human-readable description of a device
// requirement, used to report which prerequisites a host is missing.
func deviceLabel(device Device) string {
	switch device.Type {
	case "cpu":
		return fmt.Sprintf("cpu architecture %s", device.Architecture)
	case "pci", "":
		return pciLabel(device)
	case "kernel-module":
		return fmt.Sprintf("kernel module %s", device.Name)
	case "hugepages":
		if device.Num > 0 {
			return fmt.Sprintf("%d %s hugepages", device.Num, device.Size)
		}

		return fmt.Sprintf("%s hugepages", device.Size)
	case "iommu":
		return "IOMMU"
	default:
		return device.Type
	}
}

func pciLabel(device Device) string {
	if device.VendorID == "" {
		return "PCI device"
	}

	if device.DeviceID == "" {
		return fmt.Sprintf("PCI device vendor %s", device.VendorID)
	}

	return fmt.Sprintf("PCI device %s:%s", device.VendorID, device.DeviceID)
}

// matchPCI reports whether any host PCI device satisfies the given
// requirement. Vendor, device and class constraints are optional; more
// specified constraints yield a higher score.
func matchPCI(device Device, devices []PCIDevice) (int, bool) {
	vendor, hasVendor := parseID(device.VendorID)
	deviceID, hasDevice := parseID(device.DeviceID)
	class, hasClass := parseID(device.DeviceClass)

	for _, candidate := range devices {
		if hasVendor && candidate.VendorID != uint16(vendor) {
			continue
		}

		if hasDevice && candidate.DeviceID != uint16(deviceID) {
			continue
		}

		if hasClass && candidate.DeviceClass != uint16(class) {
			continue
		}

		score := 5
		if hasDevice {
			score++
		}

		if hasClass {
			score++
		}

		return score, true
	}

	return 0, false
}

// parseSize parses a memory size into its value in KiB. It accepts a bare
// number of KiB or a number with a unit suffix: k, m or g (case-insensitive,
// with optional B or iB). For example "2048kB", "2G" and "1048576" all parse
// to their value in KiB.
func parseSize(value string) (uint64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	multiplier := uint64(1)
	number := value

	end := len(value)
	for end > 0 && !isDigit(value[end-1]) {
		end--
	}

	if end < len(value) {
		number = value[:end]

		switch strings.ToUpper(value[end:]) {
		case "K", "KB", "KIB":
			multiplier = 1
		case "M", "MB", "MIB":
			multiplier = 1024
		case "G", "GB", "GIB":
			multiplier = 1024 * 1024
		default:
			return 0, false
		}
	}

	parsed, err := strconv.ParseUint(strings.TrimSpace(number), 10, 64)
	if err != nil {
		return 0, false
	}

	return parsed * multiplier, true
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// parseID parses an optional hexadecimal identifier such as "0x8086". An empty
// string means the constraint is not specified.
func parseID(value string) (uint32, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	parsed, err := strconv.ParseUint(strings.TrimPrefix(value, "0x"), 16, 32)
	if err != nil {
		return 0, false
	}

	return uint32(parsed), true
}
