package inference

import "testing"

func cpuRequirement(arch string) Device {
	return Device{Type: "cpu", Architecture: arch}
}

func pciRequirement(vendor string) Device {
	return Device{Type: "pci", VendorID: vendor}
}

func TestSelectPrefersSpecificEngine(t *testing.T) {
	engines := []Engine{
		{Name: "ovs", Devices: Devices{Anyof: []Device{cpuRequirement("amd64")}}},
		{
			Name: "ovs-doca",
			Devices: Devices{
				Allof: []Device{cpuRequirement("amd64")},
				Anyof: []Device{pciRequirement("0x15b3")},
			},
		},
	}

	hardware := &Hardware{
		Architecture: "amd64",
		PCIDevices:   []PCIDevice{{VendorID: 0x15b3, DeviceID: 0x1015, DeviceClass: 0x0200}},
	}

	selected, err := Select(engines, hardware)
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
			Name: "ovs-doca",
			Devices: Devices{
				Allof: []Device{cpuRequirement("amd64")},
				Anyof: []Device{pciRequirement("0x15b3")},
			},
		},
	}

	hardware := &Hardware{Architecture: "amd64"}

	selected, err := Select(engines, hardware)
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
