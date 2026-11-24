package inference

import "strings"

// DocaEngineName identifies the engine that provides the DOCA-accelerated
// datapath.
const DocaEngineName = "ovs-doca"

// docaPCIVendorID is the PCI vendor id of Mellanox ConnectX and BlueField
// network devices.
const docaPCIVendorID = uint16(0x15b3)

// DocaReadiness summarises how close a host is to being able to run the DOCA
// datapath.
type DocaReadiness struct {
	// Capable is true when the host has DOCA-capable hardware: an amd64 CPU
	// and a Mellanox ConnectX or BlueField NIC.
	Capable bool
	// Ready is true when every DOCA prerequisite (driver, hugepages, IOMMU)
	// is satisfied. It is only meaningful when Capable is true.
	Ready bool
	// Missing lists the DOCA prerequisites the host does not satisfy. It is
	// populated only when Capable is true and Ready is false.
	Missing []string
}

// AssessDoca evaluates the DOCA engine against the host hardware and reports
// the host's DOCA readiness. It returns a zero-value readiness (not capable)
// when no DOCA engine is present.
func AssessDoca(engines []Engine, hardware *Hardware) DocaReadiness {
	engine := findDocaEngine(engines)
	if engine == nil || !docaCapable(hardware) {
		return DocaReadiness{}
	}

	readiness := DocaReadiness{Capable: true}
	match := Score(*engine, hardware)
	readiness.Ready = match.Compatible
	readiness.Missing = match.Missing

	return readiness
}

// UseDocaDatapath reports whether the accelerated DOCA datapath should be
// used: the active engine is the DOCA engine and the host is DOCA-ready.
func UseDocaDatapath(activeEngine string, readiness DocaReadiness) bool {
	return activeEngine == DocaEngineName && readiness.Ready
}

func findDocaEngine(engines []Engine) *Engine {
	for i := range engines {
		if engines[i].Name == DocaEngineName {
			return &engines[i]
		}
	}

	return nil
}

func docaCapable(hardware *Hardware) bool {
	if hardware.Architecture != "amd64" && hardware.Architecture != "arm64" {
		return false
	}

	for _, device := range hardware.PCIDevices {
		if device.VendorID == docaPCIVendorID {
			return true
		}
	}

	return false
}

// SummariseDoca renders a one-line description of the host's DOCA readiness
// for status output. It returns an empty string when the host has no
// DOCA-capable hardware.
func (r DocaReadiness) Summarise() string {
	if !r.Capable {
		return ""
	}

	if r.Ready {
		return "ready"
	}

	return "not ready (missing: " + strings.Join(r.Missing, ", ") + ")"
}
