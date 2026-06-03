//go:build darwin

// This file implements the daemon-via-launchctl path used by `shan daemon
// start` (npm CLI standalone install). It is NOT used by the Kocoro Desktop
// bundle path — Desktop spawns the daemon directly as a child process via
// Foundation Process() and tracks lifecycle in its own DaemonManager. See
// docs/desktop-calendar-rpc.md §4.1 for the Desktop-spawn model and §1.2
// for the selection rationale.
//
// Calendar RPC and other Desktop-RPC-dependent features only function in
// the Desktop-spawn path; npm CLI users do not have those tools registered
// (the --rpc-socket / --rpc-pidfile flags are absent here, so cmd/daemon.go
// never constructs a DesktopRPCBroker and RegisterCalendarTools no-ops).
package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const daemonLabel = "com.shannon.daemon"

// DaemonPlistPath returns the standard launchd plist path for the daemon.
func DaemonPlistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", daemonLabel+".plist")
}

// GenerateDaemonPlist generates a launchd plist for running the daemon in background mode.
func GenerateDaemonPlist(shanBin, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
		<string>start</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, daemonLabel, xmlEscape(shanBin), xmlEscape(logPath), xmlEscape(logPath))
}

// WriteDaemonPlist atomically writes plist content to the given path.
func WriteDaemonPlist(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".plist-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	tmp.Close()
	return os.Rename(tmpPath, path)
}

// RemoveDaemonPlist removes the daemon plist file.
func RemoveDaemonPlist() error {
	return os.Remove(DaemonPlistPath())
}

// LaunchctlBootstrap loads the daemon plist using the modern launchctl API.
func LaunchctlBootstrap(plistPath string) error {
	uid := os.Getuid()
	target := fmt.Sprintf("gui/%d", uid)

	// Bootout first to clear stale state (ignore error)
	exec.Command("launchctl", "bootout", target+"/"+daemonLabel).Run()

	out, err := exec.Command("launchctl", "bootstrap", target, plistPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %w: %s", plistPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LaunchctlBootout unloads the daemon service.
func LaunchctlBootout() error {
	uid := os.Getuid()
	target := fmt.Sprintf("gui/%d/%s", uid, daemonLabel)
	out, err := exec.Command("launchctl", "bootout", target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootout %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsDaemonServiceLoaded checks if the daemon service is currently loaded in launchd.
func IsDaemonServiceLoaded() bool {
	uid := os.Getuid()
	target := fmt.Sprintf("gui/%d/%s", uid, daemonLabel)
	err := exec.Command("launchctl", "print", target).Run()
	return err == nil
}

// ShanBinary returns the path to the current shan executable.
func ShanBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "shan"
	}
	return exe
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	s = strings.ReplaceAll(s, "'", "&apos;")
	return s
}
