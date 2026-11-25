package inference

import (
	"slices"
	"testing"
)

func cpuRequirement(arch string) Device {
	return Device{Type: "cpu", Architecture: arch}
}

func pciRequirement(vendor string) Device {
	return Device{Type: "pci", VendorID: vendor}
}

func kernelModuleRequirement(name string) Device {
	return Device{Type: "kernel-module", Name: name}
}

func hugepagesRequirement(size string, num uint64) Device {
	return Device{Type: "hugepages", Size: size, Num: num}
}

func iommuRequirement() Device {
	return Device{Type: "iommu"}
}

func docaDevices() Devices {
	return Devices{
		Allof: []Device{
			pciRequirement("0x15b3"),
			kernelModuleRequirement("mlx5_core"),
			hugepagesRequirement("2M", 4096),
			iommuRequirement(),
		},
		Anyof: []Device{
			cpuRequirement("amd64"),
			cpuRequirement("arm64"),
		},
	}
}

func docaHardware() *Hardware {
	return &Hardware{
		Architecture:  "amd64",
		PCIDevices:    []PCIDevice{{VendorID: 0x15b3, DeviceID: 0x1015, DeviceClass: 0x0200}},
		KernelModules: []string{"mlx5_core"},
		Hugepages:     []Hugepage{{SizeKiB: 2048, Reserved: 4096}},
		IOMMUEnabled:  true,
	}
}

func dpuHardware() *Hardware {
	return &Hardware{
		Architecture:  "arm64",
		PCIDevices:    []PCIDevice{{VendorID: 0x15b3, DeviceID: 0xa2da, DeviceClass: 0x0604}},
		KernelModules: []string{"mlx5_core"},
		Hugepages:     []Hugepage{{SizeKiB: 2048, Reserved: 4096}},
		IOMMUEnabled:  true,
	}
}

func TestSelectPrefersSpecificEngine(t *testing.T) {
	engines := []Engine{
		{Name: "ovs", Devices: Devices{Anyof: []Device{cpuRequirement("amd64")}}},
		{
			Name:    "ovs-doca",
			Devices: docaDevices(),
		},
	}

	selected, err := Select(engines, docaHardware())
	if err != nil {
		t.Fatalf("Select returned error: %s", err)
	}

	if selected.Name != "ovs-doca" {
		t.Fatalf("expected ovs-doca, got %q", selected.Name)
	}
}

func TestSelectFallsBackToGenericEngine(t *testing.T) {
	engines := []Engine{
		{Name: "ovs", Devices: Devices{Anyof: []Device{cpuRequirement("amd64")}}},
		{
			Name:    "ovs-doca",
			Devices: docaDevices(),
		},
	}

	selected, err := Select(engines, &Hardware{Architecture: "amd64"})
	if err != nil {
		t.Fatalf("Select returned error: %s", err)
	}

	if selected.Name != "ovs" {
		t.Fatalf("expected ovs, got %q", selected.Name)
	}
}

func TestSelectExcludesDevelEngines(t *testing.T) {
	engines := []Engine{
		{Name: "ovs", Grade: "devel", Devices: Devices{Anyof: []Device{cpuRequirement("amd64")}}},
	}

	_, err := Select(engines, &Hardware{Architecture: "amd64"})
	if err != ErrNoCompatibleEngine {
		t.Fatalf("expected ErrNoCompatibleEngine, got %v", err)
	}
}

func TestScoreAllofRequiresEveryDevice(t *testing.T) {
	engine := Engine{
		Name:    "ovs-doca",
		Devices: Devices{Allof: []Device{cpuRequirement("amd64"), pciRequirement("0x15b3")}},
	}

	match := Score(engine, &Hardware{Architecture: "amd64"})
	if match.Compatible {
		t.Fatal("expected engine to be incompatible without the required PCI device")
	}

	if match.Score != 0 {
		t.Fatalf("expected zero score, got %d", match.Score)
	}
}

func TestScoreReportsMissingPrerequisites(t *testing.T) {
	engine := Engine{Name: "ovs-doca", Devices: docaDevices()}

	// Everything but hugepages and IOMMU is present.
	hardware := docaHardware()
	hardware.Hugepages = nil
	hardware.IOMMUEnabled = false

	match := Score(engine, hardware)
	if match.Compatible {
		t.Fatal("expected engine to be incompatible with missing prerequisites")
	}

	want := []string{"4096 2M hugepages", "IOMMU"}

	for _, missing := range want {
		if !slices.Contains(match.Missing, missing) {
			t.Errorf("expected %q in missing list, got %v", missing, match.Missing)
		}
	}

	if len(match.Missing) != len(want) {
		t.Errorf("expected %d missing prerequisites, got %d (%v)", len(want), len(match.Missing), match.Missing)
	}
}

func TestMatchKernelModule(t *testing.T) {
	device := kernelModuleRequirement("mlx5_core")

	if _, ok := matchDevice(device, &Hardware{KernelModules: []string{"mlx5_core", "tun"}}); !ok {
		t.Fatal("expected mlx5_core to match")
	}

	if _, ok := matchDevice(device, &Hardware{KernelModules: []string{"mlx4_core"}}); ok {
		t.Fatal("expected mlx5_core not to match a host without it")
	}
}

func TestMatchHugepages(t *testing.T) {
	device := hugepagesRequirement("2M", 4096)

	if _, ok := matchDevice(device, &Hardware{Hugepages: []Hugepage{{SizeKiB: 2048, Reserved: 4096}}}); !ok {
		t.Fatal("expected 4096 2M hugepages to match")
	}

	if _, ok := matchDevice(device, &Hardware{Hugepages: []Hugepage{{SizeKiB: 2048, Reserved: 4095}}}); ok {
		t.Fatal("expected fewer than 4096 2M hugepages not to match")
	}

	if _, ok := matchDevice(device, &Hardware{Hugepages: []Hugepage{{SizeKiB: 1024 * 1024, Reserved: 4096}}}); ok {
		t.Fatal("expected 1G hugepages not to match a 2M requirement")
	}
}

func TestMatchIOMMU(t *testing.T) {
	device := iommuRequirement()

	if _, ok := matchDevice(device, &Hardware{IOMMUEnabled: true}); !ok {
		t.Fatal("expected IOMMU to match when enabled")
	}

	if _, ok := matchDevice(device, &Hardware{}); ok {
		t.Fatal("expected IOMMU not to match when disabled")
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]uint64{
		"1048576": 1024 * 1024,
		"2048kB":  2048,
		"2M":      2048,
		"1G":      1024 * 1024,
		"1GiB":    1024 * 1024,
		"1g":      1024 * 1024,
	}

	for input, want := range cases {
		got, ok := parseSize(input)
		if !ok {
			t.Errorf("parseSize(%q) failed", input)
			continue
		}

		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", input, got, want)
		}
	}

	for _, input := range []string{"", "abc", "1X", "G"} {
		if _, ok := parseSize(input); ok {
			t.Errorf("parseSize(%q) unexpectedly succeeded", input)
		}
	}
}
