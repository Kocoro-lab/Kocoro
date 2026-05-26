//go:build windows

package mcp

import "github.com/mark3labs/mcp-go/client/transport"

// withProcessGroup is a no-op on Windows. Process-group semantics differ
// (Job objects rather than POSIX pgid) and Kocoro's daemon does not
// currently ship for Windows; if that changes, wire up
// JobObject-based cleanup here.
func withProcessGroup() transport.StdioOption {
	return func(*transport.Stdio) {}
}
