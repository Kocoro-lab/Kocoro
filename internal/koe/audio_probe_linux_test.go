//go:build linux

package koe

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeAudioCarrier_PerformsHelloAndClosesIdleConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audio.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		body, err := readControl(conn)
		if err != nil {
			serverDone <- err
			return
		}
		var hello helloMsg
		if err := json.Unmarshal(body, &hello); err != nil {
			serverDone <- err
			return
		}
		if hello.Type != "hello" || hello.Role != "koe" || hello.Proto != audioProto {
			serverDone <- &unexpectedAudioHello{hello: hello}
			return
		}
		peer, _ := json.Marshal(helloMsg{Type: "hello", Proto: audioProto, Role: "carrier"})
		if err := writeControl(conn, peer); err != nil {
			serverDone <- err
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		one := make([]byte, 1)
		_, err = conn.Read(one)
		serverDone <- err
	}()

	if err := ProbeAudioCarrier(path); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := <-serverDone; !errors.Is(err, io.EOF) {
		t.Fatalf("probe did not close the idle carrier connection: %v", err)
	}
}

type unexpectedAudioHello struct{ hello helloMsg }

func (e *unexpectedAudioHello) Error() string { return "unexpected audio hello" }

func TestProbeAudioCarrier_RejectsProtoMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audio.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = readControl(conn)
		peer, _ := json.Marshal(helloMsg{Type: "hello", Proto: "9.9", Role: "carrier"})
		_ = writeControl(conn, peer)
	}()

	if err := ProbeAudioCarrier(path); err == nil {
		t.Fatal("proto mismatch must fail loud")
	}
}
