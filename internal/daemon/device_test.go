package daemon

import "testing"

func TestDeviceInfo_PersistsStableID(t *testing.T) {
	dir := t.TempDir()
	first, err := loadOrCreateDeviceInfo(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := loadOrCreateDeviceInfo(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if first.DeviceID == "" {
		t.Fatal("device id is empty")
	}
	if second.DeviceID != first.DeviceID {
		t.Fatalf("device id changed: %q -> %q", first.DeviceID, second.DeviceID)
	}
	if second.Platform == "" {
		t.Fatal("platform is empty")
	}
}
