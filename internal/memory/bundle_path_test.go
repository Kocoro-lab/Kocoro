package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveFromExe_FoundWhenPresent(t *testing.T) {
	// Build a temp directory tree that mirrors the app bundle layout:
	//   base/MacOS/shan           ← simulated shan executable
	//   base/Helpers/tlm.app/Contents/MacOS/tlm
	base := t.TempDir()
	tlmBin := filepath.Join(base, "Helpers", "tlm.app", "Contents", "MacOS", "tlm")
	if err := os.MkdirAll(filepath.Dir(tlmBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tlmBin, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	exePath := filepath.Join(base, "MacOS", "shan")
	got := resolveFromExe(exePath)
	if got != filepath.Clean(tlmBin) {
		t.Fatalf("got %q want %q", got, filepath.Clean(tlmBin))
	}
}

func TestResolveFromExe_AbsentReturnsEmpty(t *testing.T) {
	base := t.TempDir() // no tlm.app inside
	got := resolveFromExe(filepath.Join(base, "MacOS", "shan"))
	if got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}
}

func TestBundleRelativeTLMPath_NoBundle(t *testing.T) {
	// In the test runner environment there is no adjacent tlm.app bundle.
	got := BundleRelativeTLMPath()
	if got != "" {
		t.Fatalf("expected empty in test env, got %q", got)
	}
}
