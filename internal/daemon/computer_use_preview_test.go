package daemon

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

func computerUsePreviewPNG(t *testing.T, width, height int) agent.ImageBlock {
	t.Helper()
	buffer := bytes.NewBuffer(nil)
	finite := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			finite.SetRGBA(x, y, color.RGBA{R: 40, G: 20, B: 90, A: 255})
		}
	}
	buffer.Reset()
	if err := png.Encode(buffer, finite); err != nil {
		t.Fatal(err)
	}
	return agent.ImageBlock{
		MediaType: "image/png",
		Data:      base64.StdEncoding.EncodeToString(buffer.Bytes()),
	}
}

func TestComputerUsePreviewStoreKeepsOnlyExactLeaseAndNormalizedCursor(t *testing.T) {
	store := NewComputerUsePreviewStore()
	if err := store.Publish("cul_a", computerUsePreviewPNG(t, 4, 3)); err != nil {
		t.Fatal(err)
	}
	if store.SetCursor("cul_a", 3, 2, "click") != true {
		t.Fatal("expected cursor update")
	}
	frame, ok := store.Snapshot("cul_a")
	if !ok || frame.Width != 4 || frame.Height != 3 ||
		frame.Cursor == nil || frame.Cursor.X != 1 || frame.Cursor.Y != 1 {
		t.Fatalf("unexpected frame: %#v", frame)
	}
	if _, ok := store.Snapshot("cul_other"); ok {
		t.Fatal("foreign lease received preview")
	}
	if err := store.Publish("cul_b", computerUsePreviewPNG(t, 2, 2)); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Snapshot("cul_a"); ok {
		t.Fatal("old lease survived replacement")
	}
	store.ClearLease("cul_b")
	if _, ok := store.Snapshot("cul_b"); ok {
		t.Fatal("cleared lease still has preview")
	}
}

func TestComputerUsePreviewHTTPRequiresPresenceAndCurrentLease(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{
		LeaseTTL: time.Hour,
		Now:      time.Now,
	})
	t.Setenv(localPresenceEnv, computerUseHTTPPresenceToken)
	server := NewServer(0, nil, nil, "test")
	server.SetComputerUseCoordinator(coordinator)
	lease, err := coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID: "session", TurnID: "turn", SourceKind: "desktop",
		SourceLabel: "Kocoro Desktop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.computerUsePreview.Publish(
		lease.LeaseID,
		computerUsePreviewPNG(t, 4, 3),
	); err != nil {
		t.Fatal(err)
	}
	server.computerUsePreview.SetCursor(lease.LeaseID, 2, 1, "click")

	request := httptest.NewRequest(
		http.MethodGet,
		"/local/computer-use/preview?lease_id="+lease.LeaseID,
		nil,
	)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing presence status = %d", response.Code)
	}

	request.Header.Set(localPresenceHeader, computerUseHTTPPresenceToken)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get(computerUsePreviewLeaseHeader) != lease.LeaseID ||
		response.Header().Get(computerUsePreviewCursorKindHeader) != "click" {
		t.Fatalf("unexpected response: status=%d headers=%v body=%s",
			response.Code, response.Header(), response.Body.String())
	}

	if err := coordinator.EndTurn(lease.TurnID, guicontrol.ComputerUseResultVerified); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("terminal lease status = %d", response.Code)
	}
	if _, ok := server.computerUsePreview.Snapshot(lease.LeaseID); ok {
		t.Fatal("terminal event did not clear preview bytes")
	}
}

func TestComputerUsePreviewStoreRejectsMalformedImages(t *testing.T) {
	store := NewComputerUsePreviewStore()
	for _, image := range []agent.ImageBlock{
		{MediaType: "text/plain", Data: base64.StdEncoding.EncodeToString([]byte("x"))},
		{MediaType: "image/png", Data: "not-base64"},
		{MediaType: "image/png", Data: base64.StdEncoding.EncodeToString([]byte("not-png"))},
	} {
		if err := store.Publish("cul_a", image); err == nil {
			t.Fatalf("accepted malformed image: %#v", image)
		}
	}
}
