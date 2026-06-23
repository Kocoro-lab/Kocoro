package tools

import (
	"errors"
	"strings"
	"testing"
)

// errReader yields its data once, then a non-EOF error — to drive readLoop's
// scanner.Err() branch without allocating a 64 MiB line.
type errReader struct {
	data []byte
	err  error
	done bool
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.done = true
	return n, nil
}

// TestReadLoopLargeResponseLine proves a multi-MB capture_window response (its
// inline base64 PNG runs well past the old 1 MiB scanner cap) is dispatched to
// the caller instead of being misreported as "unexpected EOF". Regression for
// the retina-screenshot EOF bug: a window capture EOF'd while a tiny
// window_not_found response over the same transport succeeded.
func TestReadLoopLargeResponseLine(t *testing.T) {
	c := &AXClient{pending: make(map[int64]chan AXResponse)}
	ch := make(chan AXResponse, 1)
	c.pending[1] = ch

	// ~2 MiB payload — over the old 1 MiB cap, under axMaxResponseLine.
	big := strings.Repeat("A", 2*1024*1024)
	line := `{"id":1,"result":{"ok":true,"image_base64":"` + big + `","width":10,"height":10}}` + "\n"

	c.readLoop(strings.NewReader(line))

	select {
	case resp := <-ch:
		if resp.Error != nil {
			t.Fatalf("large response misreported as error: %q", resp.Error.Message)
		}
		if len(resp.Result) == 0 {
			t.Fatal("expected non-empty result for large response")
		}
	default:
		t.Fatal("caller was never unblocked for a large response")
	}
}

// TestReadLoopReportsScannerError proves a genuine stream error is surfaced
// honestly ("read error: ...") rather than masqueraded as the clean-disconnect
// "unexpected EOF" that previously hid the oversized-capture failure.
func TestReadLoopReportsScannerError(t *testing.T) {
	c := &AXClient{pending: make(map[int64]chan AXResponse)}
	ch := make(chan AXResponse, 1)
	c.pending[1] = ch

	sentinel := errors.New("boom")
	c.readLoop(&errReader{data: []byte("partial-no-newline"), err: sentinel})

	select {
	case resp := <-ch:
		if resp.Error == nil {
			t.Fatal("expected an error for a failed stream")
		}
		if !strings.Contains(resp.Error.Message, "read error") ||
			!strings.Contains(resp.Error.Message, "boom") {
			t.Fatalf("scanner error not surfaced honestly: %q", resp.Error.Message)
		}
	default:
		t.Fatal("caller was never unblocked on stream error")
	}
}

// TestReadLoopCleanEOF keeps the disconnect path honest: a clean stream close
// with a still-pending caller must still report "unexpected EOF".
func TestReadLoopCleanEOF(t *testing.T) {
	c := &AXClient{pending: make(map[int64]chan AXResponse)}
	ch := make(chan AXResponse, 1)
	c.pending[1] = ch

	c.readLoop(strings.NewReader("")) // immediate EOF, nothing dispatched

	select {
	case resp := <-ch:
		if resp.Error == nil || resp.Error.Message != "ax_server: unexpected EOF" {
			t.Fatalf("clean EOF should report disconnect, got: %+v", resp.Error)
		}
	default:
		t.Fatal("caller was never unblocked on clean EOF")
	}
}
