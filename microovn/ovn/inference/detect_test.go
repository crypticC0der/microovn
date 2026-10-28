package inference

import (
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
