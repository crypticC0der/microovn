package inference

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestScanPCI(t *testing.T) {
	fsys := fstest.MapFS{
		"0000:00:1f.6/vendor": {Data: []byte("0x8086\n")},
		"0000:00:1f.6/device": {Data: []byte("0x15f9\n")},
		"0000:00:1f.6/class":  {Data: []byte("0x020000\n")},
		"0000:03:00.0/vendor": {Data: []byte("0x15b3\n")},
		"0000:03:00.0/device": {Data: []byte("0x1015\n")},
		"0000:03:00.0/class":  {Data: []byte("0x020000\n")},
	}

	devices, err := scanPCI(fsys)
	if err != nil {
		t.Fatalf("scanPCI returned error: %s", err)
	}

	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}

	found := false

	for _, device := range devices {
		if device.VendorID == 0x15b3 {
			found = true

			if device.DeviceID != 0x1015 {
				t.Errorf("expected device id 0x1015, got %#x", device.DeviceID)
			}

			if device.DeviceClass != 0x0200 {
				t.Errorf("expected device class 0x0200, got %#x", device.DeviceClass)
			}
		}
	}

	if !found {
		t.Fatal("expected to find the 0x15b3 device")
	}
}

func TestScanPCISkipsIncompleteDevices(t *testing.T) {
	fsys := fstest.MapFS{
		"0000:00:00.0/vendor": {Data: []byte("0x8086\n")},
	}

	devices, err := scanPCI(fsys)
	if err != nil {
		t.Fatalf("scanPCI returned error: %s", err)
	}

	if len(devices) != 0 {
		t.Fatalf("expected incomplete devices to be skipped, got %d", len(devices))
	}
}

func TestScanKernelModules(t *testing.T) {
	fsys := fstest.MapFS{
		"modules": {Data: []byte("mlx5_core 123456 0 - Live 0x0000000\ntun 40960 0 - Live 0x0000000\n")},
	}

	modules := scanKernelModules(fsys)
	if len(modules) != 2 {
		t.Fatalf("expected 2 modules, got %d", len(modules))
	}

	if modules[0] != "mlx5_core" || modules[1] != "tun" {
		t.Fatalf("expected mlx5_core and tun, got %v", modules)
	}
}

func TestScanKernelModulesUnreadable(t *testing.T) {
	if modules := scanKernelModules(fstest.MapFS{}); len(modules) != 0 {
		t.Fatalf("expected no modules from an empty filesystem, got %v", modules)
	}
}

func TestScanHugepages(t *testing.T) {
	fsys := fstest.MapFS{
		"hugepages-1048576kB/nr_hugepages": {Data: []byte("8\n")},
		"hugepages-2048kB/nr_hugepages":    {Data: []byte("0\n")},
	}

	pools := scanHugepages(fsys)
	if len(pools) != 1 {
		t.Fatalf("expected 1 reserved hugepage size, got %d (%v)", len(pools), pools)
	}

	if pools[0].SizeKiB != 1024*1024 {
		t.Fatalf("expected 1 GiB (%d KiB), got %d", 1024*1024, pools[0].SizeKiB)
	}

	if pools[0].Reserved != 8 {
		t.Fatalf("expected 8 reserved pages, got %d", pools[0].Reserved)
	}
}

func TestScanHugepagesSkipsUnreservedSizes(t *testing.T) {
	fsys := fstest.MapFS{
		"hugepages-1048576kB/nr_hugepages": {Data: []byte("0\n")},
	}

	if sizes := scanHugepages(fsys); len(sizes) != 0 {
		t.Fatalf("expected unreserved hugepage sizes to be skipped, got %v", sizes)
	}
}

func TestScanHugepagesUnreadable(t *testing.T) {
	if sizes := scanHugepages(fstest.MapFS{}); len(sizes) != 0 {
		t.Fatalf("expected no hugepages from an empty filesystem, got %v", sizes)
	}
}

func TestParseHugepageDirName(t *testing.T) {
	cases := map[string]uint64{
		"hugepages-1048576kB": 1024 * 1024,
		"hugepages-2048kB":    2048,
	}

	for name, want := range cases {
		got, ok := parseHugepageDirName(name)
		if !ok {
			t.Errorf("parseHugepageDirName(%q) failed", name)
			continue
		}

		if got != want {
			t.Errorf("parseHugepageDirName(%q) = %d, want %d", name, got, want)
		}
	}

	for _, name := range []string{"", "foo", "hugepages-", "hugepages-XkB"} {
		if _, ok := parseHugepageDirName(name); ok {
			t.Errorf("parseHugepageDirName(%q) unexpectedly succeeded", name)
		}
	}
}

func TestIOMMUEnabled(t *testing.T) {
	if iommuEnabled(fstest.MapFS{"ivhd0": {Data: []byte(""), Mode: fs.ModeDir}}) == false {
		t.Fatal("expected IOMMU enabled with a populated directory")
	}

	if iommuEnabled(fstest.MapFS{}) {
		t.Fatal("expected IOMMU disabled with an empty directory")
	}
}
