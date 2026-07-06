package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const deviceFileName = "device.json"

type DeviceInfo struct {
	DeviceID    string `json:"device_id"`
	DisplayName string `json:"display_name"`
	Platform    string `json:"platform"`
}

func loadOrCreateDeviceInfo(shannonDir string) (DeviceInfo, error) {
	host, _ := os.Hostname()
	info := DeviceInfo{
		DisplayName: strings.TrimSpace(host),
		Platform:    runtime.GOOS,
	}
	if shannonDir == "" {
		id, err := generateDeviceID()
		if err != nil {
			return DeviceInfo{}, err
		}
		info.DeviceID = id
		return info, nil
	}
	path := filepath.Join(shannonDir, deviceFileName)
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &info); err != nil {
			log.Printf("daemon: failed to parse %s; rotating device id: %v", path, err)
		}
	}
	if info.DeviceID == "" {
		id, err := generateDeviceID()
		if err != nil {
			return DeviceInfo{}, err
		}
		info.DeviceID = id
	}
	if info.DisplayName == "" {
		info.DisplayName = strings.TrimSpace(host)
	}
	if info.Platform == "" {
		info.Platform = runtime.GOOS
	}
	if err := os.MkdirAll(shannonDir, 0o700); err != nil {
		return DeviceInfo{}, err
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return DeviceInfo{}, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return DeviceInfo{}, err
	}
	return info, nil
}

func generateDeviceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate device id: %w", err)
	}
	return "dev_" + hex.EncodeToString(b[:]), nil
}
