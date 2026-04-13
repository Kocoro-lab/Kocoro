package cwdctx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// suppress unused import warning — fmt is used in table tests
var _ = fmt.Sprintf

func TestSessionCWD_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := FromContext(ctx); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	ctx = WithSessionCWD(ctx, "/projects/foo")
	if got := FromContext(ctx); got != "/projects/foo" {
		t.Fatalf("expected /projects/foo, got %q", got)
	}
}

func TestResolvePath_AbsoluteUnchanged(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	got := ResolvePath(ctx, "/etc/hosts")
	if got != "/etc/hosts" {
		t.Fatalf("absolute path should be unchanged, got %q", got)
	}
}

func TestResolvePath_TildeExpanded(t *testing.T) {
	home, _ := os.UserHomeDir()
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	got := ResolvePath(ctx, "~/Desktop")
	if got != filepath.Join(home, "Desktop") {
		t.Fatalf("expected %s, got %q", filepath.Join(home, "Desktop"), got)
	}
}

func TestResolvePath_RelativeUsesSessionCWD(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	got := ResolvePath(ctx, "src/main.go")
	if got != "/projects/foo/src/main.go" {
		t.Fatalf("expected /projects/foo/src/main.go, got %q", got)
	}
}

func TestResolvePath_DotUsesSessionCWD(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	got := ResolvePath(ctx, ".")
	if got != "/projects/foo" {
		t.Fatalf("expected /projects/foo, got %q", got)
	}
}

func TestResolvePath_EmptyPathReturnsSessionCWD(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	got := ResolvePath(ctx, "")
	if got != "/projects/foo" {
		t.Fatalf("expected /projects/foo, got %q", got)
	}
}

func TestResolvePath_NoContextFallsBackToGetwd(t *testing.T) {
	cwd, _ := os.Getwd()
	ctx := context.Background()
	got := ResolvePath(ctx, "relative.txt")
	expected := filepath.Join(cwd, "relative.txt")
	if got != expected {
		t.Fatalf("expected %s, got %q", expected, got)
	}
}

func TestIsUnderSessionCWD(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	tests := []struct {
		path string
		want bool
	}{
		{".", true},
		{"", true},
		{"src/main.go", true},
		{"/projects/foo/bar", true},
		{"/projects/foo", true},
		{"/etc/passwd", false},
		{"../../etc/passwd", false},
		{"/projects/foobar", false},
	}
	for _, tt := range tests {
		if got := IsUnderSessionCWD(ctx, tt.path); got != tt.want {
			t.Errorf("IsUnderSessionCWD(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestResolvePath_CleansTraversalInAbsolutePath(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	got := ResolvePath(ctx, "/projects/foo/../secret")
	if got != "/projects/secret" {
		t.Fatalf("expected /projects/secret, got %q", got)
	}
}

func TestIsUnderSessionCWD_RejectsAbsoluteTraversal(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	// Absolute path that traverses out of CWD
	if IsUnderSessionCWD(ctx, "/projects/foo/../secret") {
		t.Error("/projects/foo/../secret should NOT be under /projects/foo after cleaning")
	}
	if IsUnderSessionCWD(ctx, "/projects/foo/../../etc/passwd") {
		t.Error("traversal via absolute path should be rejected")
	}
	// But legitimate subpath should still work
	if !IsUnderSessionCWD(ctx, "/projects/foo/sub/../sub/file.txt") {
		t.Error("/projects/foo/sub/../sub/file.txt should be under CWD after cleaning")
	}
}

func TestResolveEffectiveCWD_Priority(t *testing.T) {
	tests := []struct {
		name    string
		request string
		session string
		agent   string
		want    string
	}{
		{"request wins", "/req", "/sess", "/agent", "/req"},
		{"session wins when no request", "", "/sess", "/agent", "/sess"},
		{"agent wins when no request or session", "", "", "/agent", "/agent"},
		{"empty when nothing provided", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveEffectiveCWD(tt.request, tt.session, tt.agent)
			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestValidateCWD_EmptyIsOK(t *testing.T) {
	if err := ValidateCWD(""); err != nil {
		t.Fatalf("empty cwd should be valid (means fallback), got %v", err)
	}
}

func TestValidateCWD_RejectsRelative(t *testing.T) {
	if err := ValidateCWD("relative/path"); err == nil {
		t.Fatal("expected error for relative path")
	}
}

func TestValidateCWD_RejectsNonexistent(t *testing.T) {
	if err := ValidateCWD("/nonexistent/path/12345"); err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestValidateCWD_AcceptsRealDir(t *testing.T) {
	dir := t.TempDir()
	if err := ValidateCWD(dir); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestValidateCWD_RejectsFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir.txt")
	os.WriteFile(file, []byte("x"), 0644)
	if err := ValidateCWD(file); err == nil {
		t.Fatal("expected error for file path")
	}
}

// --- ResolveFilesystemPath: refuse relative paths without session CWD ---

func TestResolveFilesystemPath_RelativeErrorsWhenNoSessionCWD(t *testing.T) {
	ctx := context.Background()
	_, err := ResolveFilesystemPath(ctx, "relative.txt")
	if err == nil {
		t.Fatal("expected error for relative path with no session CWD")
	}
	if !errors.Is(err, ErrNoSessionCWD) {
		t.Errorf("expected ErrNoSessionCWD, got %v", err)
	}
}

func TestResolveFilesystemPath_EmptyErrorsWhenNoSessionCWD(t *testing.T) {
	ctx := context.Background()
	if _, err := ResolveFilesystemPath(ctx, ""); err == nil {
		t.Fatal("expected error for empty path with no session CWD")
	}
	if _, err := ResolveFilesystemPath(ctx, "."); err == nil {
		t.Fatal("expected error for dot path with no session CWD")
	}
}

func TestResolveFilesystemPath_AbsoluteWorksWithoutSessionCWD(t *testing.T) {
	ctx := context.Background()
	got, err := ResolveFilesystemPath(ctx, "/etc/hosts")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/etc/hosts" {
		t.Errorf("expected /etc/hosts, got %q", got)
	}
}

func TestResolveFilesystemPath_TildeWorksWithoutSessionCWD(t *testing.T) {
	home, _ := os.UserHomeDir()
	ctx := context.Background()
	got, err := ResolveFilesystemPath(ctx, "~/Desktop")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != filepath.Join(home, "Desktop") {
		t.Errorf("expected %s, got %q", filepath.Join(home, "Desktop"), got)
	}
}

func TestResolveFilesystemPath_RelativeJoinsSessionCWD(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	got, err := ResolveFilesystemPath(ctx, "src/main.go")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/projects/foo/src/main.go" {
		t.Errorf("expected /projects/foo/src/main.go, got %q", got)
	}
}

func TestResolveFilesystemPath_DotReturnsSessionCWD(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	got, err := ResolveFilesystemPath(ctx, ".")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/projects/foo" {
		t.Errorf("expected /projects/foo, got %q", got)
	}
}

func TestResolveFilesystemPath_CleansTraversal(t *testing.T) {
	ctx := WithSessionCWD(context.Background(), "/projects/foo")
	got, err := ResolveFilesystemPath(ctx, "../bar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/projects/bar" {
		t.Errorf("expected /projects/bar, got %q", got)
	}
}
