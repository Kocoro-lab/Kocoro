package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type displayTopologyAXCaller struct {
	result json.RawMessage
	err    error
	method string
	params any
}

func (caller *displayTopologyAXCaller) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	caller.method = method
	caller.params = params
	return caller.result, caller.err
}

func TestReadDisplayTopologyV1UsesTypedRPCAndStrictDecoder(t *testing.T) {
	caller := &displayTopologyAXCaller{
		result: loadCoordinateFixture(t, "display_topology.mixed_horizontal.v1.json"),
	}
	topology, err := ReadDisplayTopologyV1(context.Background(), caller)
	if err != nil {
		t.Fatal(err)
	}
	if caller.method != "display_topology" {
		t.Fatalf("RPC method = %q, want display_topology", caller.method)
	}
	params, ok := caller.params.(map[string]any)
	if !ok || len(params) != 0 {
		t.Fatalf("RPC params = %#v, want empty object", caller.params)
	}
	if topology.TopologyID != "topo_mixed_001" || topology.Generation != 7 || len(topology.Displays) != 2 {
		t.Fatalf("typed topology lost authority or displays: %+v", topology)
	}

	var object map[string]any
	if err := json.Unmarshal(caller.result, &object); err != nil {
		t.Fatal(err)
	}
	object["unexpected"] = true
	caller.result, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDisplayTopologyV1(context.Background(), caller); err == nil {
		t.Fatal("typed RPC accepted unknown response field")
	}
}

func TestReadDisplayTopologyV1PropagatesRPCFailure(t *testing.T) {
	caller := &displayTopologyAXCaller{err: errors.New("collector failed")}
	if _, err := ReadDisplayTopologyV1(context.Background(), caller); err == nil ||
		!strings.Contains(err.Error(), "collector failed") {
		t.Fatalf("RPC error = %v", err)
	}
}

func TestAXClientMutationCallWaitsForHelperAcknowledgementAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writer := &axMutationTestWriter{}
	client := axMutationTestClient(writer)
	release := make(chan struct{})
	writer.afterWrite = func(request []byte) {
		cancel()
		go func() {
			<-release
			var envelope AXRequest
			if err := json.Unmarshal(request, &envelope); err != nil {
				return
			}
			client.pendingMu.Lock()
			response := client.pending[envelope.ID]
			client.pendingMu.Unlock()
			response <- AXResponse{ID: envelope.ID, Result: json.RawMessage(`{"result":"pressed"}`)}
		}()
	}

	type outcome struct {
		result json.RawMessage
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := client.callMutationWithAckTimeout(
			ctx, "semantic_press", map[string]any{"pid": 42}, time.Second)
		done <- outcome{result: result, err: err}
	}()

	select {
	case got := <-done:
		t.Fatalf("mutation returned before helper acknowledgement: result=%s err=%v", got.result, got.err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("acknowledged mutation error = %v", got.err)
		}
		if string(got.result) != `{"result":"pressed"}` {
			t.Fatalf("acknowledged result = %s", got.result)
		}
	case <-time.After(time.Second):
		t.Fatal("mutation did not return after helper acknowledgement")
	}
	if writer.writeCount() != 1 {
		t.Fatalf("mutation wrote %d requests, want exactly one", writer.writeCount())
	}
}

func TestAXClientMutationCallTimeoutIsBoundedTypedCommitUnknown(t *testing.T) {
	writer := &axMutationTestWriter{}
	client := axMutationTestClient(writer)
	started := time.Now()
	_, err := client.callMutationWithAckTimeout(
		context.Background(), "semantic_press", map[string]any{"pid": 42}, 25*time.Millisecond)
	elapsed := time.Since(started)

	var commitUnknown *AXMutationCommitUnknownError
	if !errors.As(err, &commitUnknown) {
		t.Fatalf("timeout error %T %v is not typed commit-unknown", err, err)
	}
	if commitUnknown.Method != "semantic_press" || commitUnknown.RetrySafe() || !commitUnknown.CommitUnknown() {
		t.Fatalf("typed timeout lost mutation policy: %+v", commitUnknown)
	}
	if elapsed < 20*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("mutation acknowledgement timeout elapsed %v, want bounded wait", elapsed)
	}
	if writer.writeCount() != 1 {
		t.Fatalf("timed out mutation wrote %d requests, want exactly one", writer.writeCount())
	}
}

func TestAXClientMutationCallPreCancelledDoesNotWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer := &axMutationTestWriter{}
	client := axMutationTestClient(writer)

	_, err := client.callMutationWithAckTimeout(
		ctx, "semantic_press", map[string]any{"pid": 42}, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel error = %v", err)
	}
	if writer.writeCount() != 0 {
		t.Fatalf("pre-cancelled mutation wrote %d requests", writer.writeCount())
	}
}

func TestAXMutationMethodClassificationCoversHelperSideEffects(t *testing.T) {
	for _, method := range []string{
		"semantic_press", "click", "press", "set_value", "mouse_event",
		"key_event", "type_text", "scroll", "focus", "launch_app",
		"prepare_task_app", "request_permission",
	} {
		if !isAXMutationMethod(method) {
			t.Errorf("method %q was not classified as a mutation", method)
		}
	}
	for _, method := range []string{
		"ping", "display_topology", "capture_coordinate_window", "capture_coordinate_display", "read_tree", "get_value",
		"find", "resolve_pid", "frontmost", "list_windows", "wait_for", "annotate",
		"capture_window", "check_permissions",
	} {
		if isAXMutationMethod(method) {
			t.Errorf("read method %q was classified as a mutation", method)
		}
	}
	if !isAXMutationMethod("future_unclassified_helper_rpc") {
		t.Fatal("unknown helper RPC did not fail closed into mutation acknowledgement semantics")
	}
}

type axMutationTestWriter struct {
	mu         sync.Mutex
	writes     int
	afterWrite func([]byte)
}

func (writer *axMutationTestWriter) Write(data []byte) (int, error) {
	request := append([]byte(nil), data...)
	writer.mu.Lock()
	writer.writes++
	afterWrite := writer.afterWrite
	writer.mu.Unlock()
	if afterWrite != nil {
		afterWrite(request)
	}
	return len(data), nil
}

func (writer *axMutationTestWriter) Close() error { return nil }

func (writer *axMutationTestWriter) writeCount() int {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.writes
}

func axMutationTestClient(writer io.WriteCloser) *AXClient {
	return &AXClient{
		writer: writer, started: true,
		pending: make(map[int64]chan AXResponse),
	}
}

// errReader yields its data once, then a non-EOF error — to drive readLoop's
// scanner.Err() branch without allocating a 64 MiB line.
type errReader struct {
	data []byte
	err  error
	done bool
}

func TestRegisterBundledApplicationUsesLaunchServices(t *testing.T) {
	const bundlePath = "/tmp/Kocoro AX Dev.app"
	var gotName string
	var gotArgs []string

	err := registerBundledApplication(bundlePath, func(name string, args ...string) ([]byte, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotName != launchServicesRegisterPath {
		t.Fatalf("registration executable = %q, want %q", gotName, launchServicesRegisterPath)
	}
	wantArgs := []string{"-f", "-R", "-trusted", bundlePath}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("registration args = %q, want %q", gotArgs, wantArgs)
	}
}

func TestAXServerPathsUsesConfiguredStandaloneBundle(t *testing.T) {
	bundlePath := filepath.Join(t.TempDir(), "Kocoro AX Dev.app")
	binPath := filepath.Join(bundlePath, "Contents", "MacOS", "ax_server")
	if err := os.MkdirAll(filepath.Dir(binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binPath, []byte("test"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(AXServerBundlePathEnv, bundlePath)

	gotBin, gotBundle, err := AXServerPaths()
	if err != nil {
		t.Fatal(err)
	}
	if gotBin != binPath || gotBundle != bundlePath {
		t.Fatalf("AXServerPaths() = (%q, %q), want (%q, %q)", gotBin, gotBundle, binPath, bundlePath)
	}
}

func TestAXServerPathsRejectsInvalidConfiguredBundle(t *testing.T) {
	t.Setenv(AXServerBundlePathEnv, filepath.Join(t.TempDir(), "missing.app"))

	_, _, err := AXServerPaths()
	if err == nil || !strings.Contains(err.Error(), AXServerBundlePathEnv+" executable") {
		t.Fatalf("AXServerPaths() error = %v, want configured-bundle executable error", err)
	}
}

func TestAXServerPathsRejectsRelativeConfiguredBundle(t *testing.T) {
	t.Setenv(AXServerBundlePathEnv, "Kocoro AX Dev.app")

	_, _, err := AXServerPaths()
	if err == nil || !strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("AXServerPaths() error = %v, want absolute-path error", err)
	}
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
