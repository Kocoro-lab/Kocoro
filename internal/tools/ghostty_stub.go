//go:build !darwin

package tools

import (
	"context"
	"fmt"
)

const minGhosttyVersion = "1.3.0"

var errNotDarwin = fmt.Errorf("ghostty integration requires macOS")

func ghosttyAvailable(context.Context) bool { return false }
func GhosttyAvailable() bool                { return false }
func ghosttyNewTab(context.Context, string, string, string) (int, int, error) {
	return 0, 0, errNotDarwin
}
func ghosttyNewSplit(context.Context, string, string, string, string) (int, int, error) {
	return 0, 0, errNotDarwin
}
func ghosttySendInput(context.Context, int, int, string) error             { return errNotDarwin }
func SetGhosttyTabAppearance(agentName string)                             {}
func ghosttyWorkspaceScript(shanBinary string, agentNames []string) string { return "" }
func GhosttyWorkspaceScript(shanBinary string, agentNames []string) string { return "" }
func ExecGhosttyScript(script string) error                                { return errNotDarwin }
