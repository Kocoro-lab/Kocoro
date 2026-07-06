package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestDeviceInfo_LogsCorruptDeviceFileBeforeRotating(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, deviceFileName), []byte(`{"device_id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	info, err := loadOrCreateDeviceInfo(dir)
	if err != nil {
		t.Fatalf("load corrupt device file: %v", err)
	}
	if info.DeviceID == "" {
		t.Fatal("device id is empty")
	}
	if !strings.Contains(buf.String(), "failed to parse") {
		t.Fatalf("log output = %q, want parse diagnostic", buf.String())
	}
}
