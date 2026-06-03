// Command fake_desktop is a Calendar RPC v1 stand-in for Kocoro Desktop.
//
// It dials a daemon's Unix domain socket, performs the spec §4.1.1
// reconciliation handshake (sends `system.capabilities`, sanity-checks the
// returned version), declares itself online via `desktop_event { desktop_online }`,
// and then sits in a read loop answering calendar.* RPCs with canned fixture
// results.
//
// Purpose:
//
//   - Lets the Daemon team smoke-test the RPC channel without needing a real
//     Kocoro Desktop build.
//   - Lets the Desktop team verify their Swift side against a reference
//     implementation that uses the same wire format.
//   - Anchors the spec §6 Phase 1 convergence checkpoint as a manual test.
//
// Usage:
//
//	# Daemon already running with --rpc-socket /path/to/daemon.sock
//	go run ./cmd/fake_desktop /path/to/daemon.sock
//
// The binary will print everything it sends and receives to stdout. Ctrl-C
// to exit. Multiple invocations are rejected by the daemon's single-instance
// guard (only one Desktop client at a time).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/daemon/desktop_rpc"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fake_desktop <sock-path>")
		os.Exit(2)
	}
	sockPath := os.Args[1]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Printf("fake_desktop: received %s, shutting down", s)
		cancel()
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		log.Fatalf("fake_desktop: dial %s: %v", sockPath, err)
	}
	defer conn.Close()
	log.Printf("fake_desktop: connected to %s", sockPath)

	// One persistent bufio.Reader for the conn lifetime — separate ones
	// per read would silently drop bytes that landed in the discarded buffer.
	reader := bufio.NewReaderSize(conn, 64*1024)

	if err := reconcile(conn, reader); err != nil {
		log.Fatalf("fake_desktop: reconciliation failed: %v", err)
	}
	if err := serve(ctx, conn, reader); err != nil {
		log.Printf("fake_desktop: serve exited: %v", err)
	}
}

// reconcile performs spec §4.1.1 steps 2b / 2c — send system.capabilities,
// check version, then declare online. Read deadlines on the conn handle
// timeouts; no ctx parameter (signal handler in main() closes conn instead).
func reconcile(conn net.Conn, reader *bufio.Reader) error {
	// 1. Send system.capabilities request.
	capsReq := &desktop_rpc.RPCRequest{
		RequestID: "drpc_fake-caps-1",
		Method:    desktop_rpc.MethodSystemCapabilities,
		Params:    json.RawMessage(`{}`),
		TimeoutMs: 3000,
		TS:        time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeFramePretty(conn, "→", desktop_rpc.EncodeRequestFrame, capsReq); err != nil {
		return fmt.Errorf("send capabilities: %w", err)
	}

	// 2. Read response.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resFrame, err := readFramePretty(reader, "←")
	if err != nil {
		return fmt.Errorf("read capabilities reply: %w", err)
	}
	conn.SetReadDeadline(time.Time{})
	if resFrame.Type != desktop_rpc.FrameDesktopRPCResult {
		return fmt.Errorf("expected result frame, got %q", resFrame.Type)
	}
	var res desktop_rpc.RPCResult
	if err := json.Unmarshal(resFrame.Payload, &res); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	if !res.OK {
		return fmt.Errorf("daemon capabilities returned error: %+v", res.Error)
	}
	var caps desktop_rpc.SystemCapabilitiesResult
	if err := json.Unmarshal(res.Result, &caps); err != nil {
		return fmt.Errorf("decode caps body: %w", err)
	}

	// 3. Compare version.
	if caps.Version != desktop_rpc.ProtocolVersion {
		return fmt.Errorf("protocol version mismatch: daemon=%q, fake_desktop=%q", caps.Version, desktop_rpc.ProtocolVersion)
	}
	log.Printf("fake_desktop: protocol version OK (%s); daemon platform: %s %s app=%s",
		caps.Version, caps.Platform.OS, caps.Platform.OSVersion, caps.Platform.AppVersion)

	// 4. Send desktop_online event.
	platform := desktop_rpc.Platform{
		OS:         "macOS",
		OSVersion:  "14.4.1",
		AppVersion: "fake_desktop-0.1",
	}
	onlineData, _ := json.Marshal(map[string]any{
		"version":  desktop_rpc.ProtocolVersion,
		"platform": platform,
	})
	evt := &desktop_rpc.DesktopEvent{
		Event: desktop_rpc.EventDesktopOnline,
		Data:  onlineData,
		TS:    time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeFramePretty(conn, "→", desktop_rpc.EncodeEventFrame, evt); err != nil {
		return fmt.Errorf("send desktop_online: %w", err)
	}
	log.Print("fake_desktop: reconciliation complete; entering serve loop")
	return nil
}

// serve reads frames in a loop and answers calendar.* RPCs with canned data.
func serve(ctx context.Context, conn net.Conn, reader *bufio.Reader) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		f, err := readFramePretty(reader, "←")
		if err != nil {
			return err
		}
		switch f.Type {
		case desktop_rpc.FrameDesktopRPCRequest:
			var req desktop_rpc.RPCRequest
			if err := json.Unmarshal(f.Payload, &req); err != nil {
				log.Printf("fake_desktop: malformed request: %v", err)
				continue
			}
			res := handleRequest(&req)
			if err := writeFramePretty(conn, "→", desktop_rpc.EncodeResultFrame, res); err != nil {
				return fmt.Errorf("send result: %w", err)
			}
		default:
			log.Printf("fake_desktop: unexpected frame type %q (ignoring)", f.Type)
		}
	}
}

// handleRequest produces canned responses for known methods, or an
// `internal_error` for anything we haven't implemented yet.
func handleRequest(req *desktop_rpc.RPCRequest) *desktop_rpc.RPCResult {
	res := &desktop_rpc.RPCResult{RequestID: req.RequestID, OK: true}
	switch req.Method {
	case desktop_rpc.MethodSystemPing:
		var p desktop_rpc.SystemPingParams
		_ = json.Unmarshal(req.Params, &p)
		body, _ := json.Marshal(desktop_rpc.SystemPingResult{
			Pong:       p.Echo,
			ServerTime: time.Now().UTC().Format(time.RFC3339Nano),
		})
		res.Result = body

	case desktop_rpc.MethodCalendarCheckPermission:
		res.Result = json.RawMessage(`{"status":"granted"}`)

	case desktop_rpc.MethodCalendarListSources:
		res.Result = json.RawMessage(`{"sources":[
			{"id":"cal_fake_icloud","title":"iCloud","account_type":"icloud","color_hex":"#3478F6","writable":true,"default_for_new_events":true},
			{"id":"cal_fake_google","title":"Work (Google)","account_type":"google","color_hex":"#EA4335","writable":true,"default_for_new_events":false}
		]}`)

	case desktop_rpc.MethodCalendarListEvents:
		res.Result = json.RawMessage(`{"events":[
			{"id":"evt_fake_1","calendar_id":"cal_fake_icloud","title":"Q2 Review (fake)","start":"2026-05-26T14:00:00+08:00","end":"2026-05-26T15:00:00+08:00","all_day":false,"location":null,"notes":null,"url":null,"is_recurring":false,"is_recurring_instance":false,"series_master_id":null,"attendees":[],"organizer_email":null,"has_alarms":false}
		],"truncated":false}`)

	case desktop_rpc.MethodCalendarGetEvent,
		desktop_rpc.MethodCalendarCreateEvent,
		desktop_rpc.MethodCalendarUpdateEvent,
		desktop_rpc.MethodCalendarDeleteEvent,
		desktop_rpc.MethodCalendarRequestPermission:
		// Echo a minimal success so smoke tests can exercise the full path.
		// Real Desktop will return richer data.
		res.Result = json.RawMessage(`{"id":"evt_fake_new","pending_remote_sync":true,"invitations_sent":false,"status":"granted","ok":true}`)

	default:
		res.OK = false
		res.Result = nil
		res.Error = &desktop_rpc.RPCError{
			Code:      desktop_rpc.ErrCodeInvalidArgument,
			Message:   "fake_desktop does not implement this method",
			Retriable: false,
		}
	}
	return res
}

// writeFramePretty marshals via the appropriate encoder, prints the JSON
// payload to stdout (for human debugging), and writes the framed bytes.
func writeFramePretty[T any](conn net.Conn, arrow string, encode func(*T) (*desktop_rpc.Frame, error), v *T) error {
	frame, err := encode(v)
	if err != nil {
		return err
	}
	log.Printf("%s %s %s", arrow, frame.Type, string(frame.Payload))
	return desktop_rpc.WriteFrame(conn, frame)
}

// readFramePretty reads one frame from the persistent reader and prints it.
func readFramePretty(reader *bufio.Reader, arrow string) (*desktop_rpc.Frame, error) {
	f, err := desktop_rpc.ReadFrame(reader)
	if err != nil {
		return nil, err
	}
	log.Printf("%s %s %s", arrow, f.Type, string(f.Payload))
	return f, nil
}
