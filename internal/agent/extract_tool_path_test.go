package agent

import "testing"

// TestExtractToolPath_GenericFilePathFallback pins the default branch: any
// tool outside the explicit name map is recognized through the conventional
// `file_path` argument (x_upload_media relies on this so user-attached files
// ride the attachment auto-approve), while the looser `path` key is
// deliberately NOT generalized.
func TestExtractToolPath_GenericFilePathFallback(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args string
		want string
	}{
		{"unknown tool with file_path", "x_upload_media", `{"file_path":"/tmp/a.png"}`, "/tmp/a.png"},
		{"unknown tool with only path", "some_tool", `{"path":"/tmp/a.png"}`, ""},
		{"unknown tool without path args", "some_tool", `{"query":"hi"}`, ""},
		{"explicit map still wins", "file_read", `{"path":"/tmp/b.txt"}`, "/tmp/b.txt"},
		{"explicit map file_path alias", "file_read", `{"file_path":"/tmp/c.txt"}`, "/tmp/c.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractToolPath(tc.tool, tc.args); got != tc.want {
				t.Errorf("extractToolPath(%q, %s) = %q, want %q", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}
