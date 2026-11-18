// Package inference implements hardware-aware selection of the Open vSwitch
// implementation used by MicroOVN.
//
// The design mirrors the engine manifest and snap component model used by the
// Canonical inference snaps: an "engine" describes an OVS variant and the
// hardware it targets, and the most suitable engine is selected automatically
// from the hardware present on the host.
package inference

// Device describes a single hardware requirement of an engine.
//
// Only a subset of the inference snap device schema is currently interpreted:
// CPU architecture, PCI vendor/device/class identifiers, loaded kernel
// modules, reserved hugepage sizes and IOMMU enablement.
type Device struct {
	Type         string `yaml:"type"`
	Bus          string `yaml:"bus,omitempty"`
	Architecture string `yaml:"architecture,omitempty"`
	VendorID     string `yaml:"vendor-id,omitempty"`
	DeviceID     string `yaml:"device-id,omitempty"`
	DeviceClass  string `yaml:"device-class,omitempty"`
	Name         string `yaml:"name,omitempty"`
	Size         string `yaml:"size,omitempty"`
}

// Devices groups hardware requirements into mandatory (Allof) and alternative
// (Anyof) sets, matching the inference snap engine manifest schema.
type Devices struct {
	Allof []Device `yaml:"allof,omitempty"`
	Anyof []Device `yaml:"anyof,omitempty"`
}

// Engine is a selectable Open vSwitch implementation together with the
// hardware it targets and the snap components it provides.
type Engine struct {
	Name       string   `yaml:"name"`
	Summary    string   `yaml:"summary,omitempty"`
	Vendor     string   `yaml:"vendor,omitempty"`
	Grade      string   `yaml:"grade,omitempty"`
	Devices    Devices  `yaml:"devices"`
	Runtime    string   `yaml:"runtime,omitempty"`
	Components []string `yaml:"components"`
}
