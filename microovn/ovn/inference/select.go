package inference

import (
	"errors"
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
}

// Score evaluates an engine against the host hardware.
//
// It mirrors the filtering and ranking steps of inference snap engine
// selection: mandatory requirements (Allof) must all be satisfied and
// alternative requirements (Anyof) must have at least one match. The score
// reflects how specifically the requirements match the host, so that engines
// targeting more specific hardware rank above generic ones.
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
			match.Score = 0
			return match
		}

		match.Score += score
	}

	if alternatives > 0 {
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
			match.Score = 0
			return match
		}
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
	default:
		return 0, false
	}
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
