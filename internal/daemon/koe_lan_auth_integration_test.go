package daemon

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// firstNonLoopbackIPv4 returns the machine's first non-loopback IPv4 (its LAN
// address), or "" if none — so the test can connect to the daemon over the real
// network path instead of loopback.
func firstNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return ""
}

// TestKoeLANAuth_RealNetworkPath binds all interfaces and drives the middleware
// over a REAL TCP connection from the machine's LAN IP — so RemoteAddr is a
// genuine non-loopback address the kernel assigns, not a synthetic httptest one.
// This is the self-verify that the LAN gate holds on the wire (the CM4 → Mac
// path in miniature) before Koe on the robot connects for real.
func TestKoeLANAuth_RealNetworkPath(t *testing.T) {
	lanIP := firstNonLoopbackIPv4()
	if lanIP == "" {
		t.Skip("no non-loopback IPv4 interface available")
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // sentinel: request reached the handler
	})
	ln, err := net.Listen("tcp", ":0") // all interfaces, like the LAN-exposed daemon
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: withKoeLANAuth([]string{"s3cr3t"}, inner)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	client := &http.Client{Timeout: 3 * time.Second}
	get := func(url, auth string) int {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		var resp *http.Response
		// The listener's goroutine may not be serving on the very first dial.
		for i := 0; i < 20; i++ {
			resp, err = client.Do(req)
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	lan := func(path string) string { return fmt.Sprintf("http://%s:%d%s", lanIP, port, path) }
	loop := func(path string) string { return fmt.Sprintf("http://127.0.0.1:%d%s", port, path) }

	if got := get(lan("/koe/realtime/mint"), ""); got != http.StatusUnauthorized {
		t.Fatalf("LAN without bearer = %d, want 401", got)
	}
	if got := get(lan("/koe/realtime/mint"), "Bearer s3cr3t"); got != http.StatusTeapot {
		t.Fatalf("LAN with correct bearer = %d, want 418 (reached handler)", got)
	}
	if got := get(loop("/koe/realtime/mint"), ""); got != http.StatusTeapot {
		t.Fatalf("loopback without bearer = %d, want 418 (exempt, reached handler)", got)
	}
}
