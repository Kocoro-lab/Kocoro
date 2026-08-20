//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

type recordingSampleWriter struct {
	writes  atomic.Int32
	data    chan []byte
	onWrite func()
}

func (w *recordingSampleWriter) WriteSample(sample media.Sample) error {
	w.writes.Add(1)
	if w.onWrite != nil {
		w.onWrite()
	}
	select {
	case w.data <- append([]byte(nil), sample.Data...):
	default:
	}
	return nil
}

func TestQwenVideoSourceAddsH264TrackOnlyToQwen(t *testing.T) {
	source := &RealtimeVideoSource{
		Codec: VideoCodecH264,
		ReadFrame: func(context.Context) ([]byte, error) {
			return []byte{0x10, 0x00, 0x00, 0x00}, nil
		},
	}

	qwen, err := newPeerConnectionForProviderWithVideo(nil, ProviderQwen, source)
	if err != nil {
		t.Fatalf("new Qwen peer: %v", err)
	}
	defer qwen.Close()
	qwenOffer, err := qwen.pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create Qwen offer: %v", err)
	}
	if !strings.Contains(qwenOffer.SDP, "m=video") || !strings.Contains(qwenOffer.SDP, "H264/90000") || !strings.Contains(qwenOffer.SDP, "profile-level-id=42e01f") {
		t.Fatalf("Qwen offer missing compatible H264 video track:\n%s", qwenOffer.SDP)
	}

	openAI, err := newPeerConnectionForProviderWithVideo(nil, ProviderOpenAI, source)
	if err != nil {
		t.Fatalf("new OpenAI peer: %v", err)
	}
	defer openAI.Close()
	openAIOffer, err := openAI.pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create OpenAI offer: %v", err)
	}
	if strings.Contains(openAIOffer.SDP, "m=video") {
		t.Fatalf("OpenAI offer unexpectedly contains Qwen video track:\n%s", openAIOffer.SDP)
	}
}

func TestQwenVideoPumpReadsOnlyDuringActiveCall(t *testing.T) {
	var active atomic.Bool
	var reads atomic.Int32
	order := make(chan string, qwenAudioPrimerFrames+1)
	recordOrder := func(kind string) {
		select {
		case order <- kind:
		default:
		}
	}
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	audioWriter := &recordingSampleWriter{onWrite: func() { recordOrder("audio") }}
	videoWriter := &recordingSampleWriter{
		data:    make(chan []byte, 1),
		onWrite: func() { recordOrder("video") },
	}
	rc := &RealtimeConn{
		audio:      audio,
		sendTrack:  audioWriter,
		videoTrack: videoWriter,
		videoSource: &RealtimeVideoSource{
			Codec:         VideoCodecH264,
			FrameInterval: 5 * time.Millisecond,
			ReadFrame: func(context.Context) ([]byte, error) {
				reads.Add(1)
				return []byte{0x00, 0x00, 0x01, 0x65}, nil
			},
		},
		callActive: active.Load,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rc.pumpVideoTrack(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	time.Sleep(20 * time.Millisecond)
	if got := reads.Load(); got != 0 {
		cancel()
		<-done
		t.Fatalf("idle video source reads = %d, want 0", got)
	}
	active.Store(true)
	waitUntil(t, func() bool { return videoWriter.writes.Load() > 0 }, "active call did not send a video frame")
	for primerFrame := 0; primerFrame < qwenAudioPrimerFrames; primerFrame++ {
		if got := <-order; got != "audio" {
			t.Fatalf("media write %d = %q, want audio primer", primerFrame, got)
		}
	}
	if got := <-order; got != "video" {
		t.Fatalf("first post-primer media write = %q, want video", got)
	}
	select {
	case frame := <-videoWriter.data:
		if string(frame) != string([]byte{0x00, 0x00, 0x01, 0x65}) {
			t.Fatalf("video frame = %v", frame)
		}
	default:
		t.Fatal("video write did not carry frame bytes")
	}
	active.Store(false)
	time.Sleep(10 * time.Millisecond)
	idleReads, idleWrites := reads.Load(), videoWriter.writes.Load()
	time.Sleep(20 * time.Millisecond)
	if got := reads.Load(); got != idleReads {
		t.Fatalf("ended call video source reads advanced from %d to %d", idleReads, got)
	}
	if got := videoWriter.writes.Load(); got != idleWrites {
		t.Fatalf("ended call video writes advanced from %d to %d", idleWrites, got)
	}
}

func TestRealtimeVideoSourceClampsFrameInterval(t *testing.T) {
	if got := (&RealtimeVideoSource{FrameInterval: time.Millisecond}).frameInterval(); got != minRealtimeVideoFrameInterval {
		t.Fatalf("frame interval = %v, want %v", got, minRealtimeVideoFrameInterval)
	}
	if got := (&RealtimeVideoSource{}).frameInterval(); got != defaultRealtimeVideoFrameInterval {
		t.Fatalf("default frame interval = %v, want %v", got, defaultRealtimeVideoFrameInterval)
	}
}

func TestQwenVideoFrameRequiresAnnexB(t *testing.T) {
	for name, frame := range map[string][]byte{
		"three-byte": {0, 0, 1, 0x65},
		"four-byte":  {0, 0, 0, 1, 0x65},
	} {
		t.Run(name, func(t *testing.T) {
			if !isAnnexBH264(frame) {
				t.Fatalf("valid Annex-B frame rejected: %v", frame)
			}
		})
	}
	if isAnnexBH264([]byte{0, 0, 0, 4, 0x65}) {
		t.Fatal("AVCC length-prefixed frame accepted as Annex-B")
	}
}

func TestQwenVideoSourceRequiresCallActive(t *testing.T) {
	_, err := ConnectQwen(
		context.Background(), nil, nil, "", nil, nil,
		ConnectOptions{VideoSource: &RealtimeVideoSource{
			Codec:     VideoCodecH264,
			ReadFrame: func(context.Context) ([]byte, error) { return []byte{1}, nil },
		}},
	)
	if err == nil || !strings.Contains(err.Error(), "realtime video source requires CallActive") {
		t.Fatalf("ConnectQwen error = %v", err)
	}
}

func TestQwenVideoReadUsesFrameIntervalDeadline(t *testing.T) {
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	readDone := make(chan error, 1)
	rc := &RealtimeConn{
		audio:      audio,
		sendTrack:  &recordingSampleWriter{},
		videoTrack: &recordingSampleWriter{},
		videoSource: &RealtimeVideoSource{
			Codec:         VideoCodecH264,
			FrameInterval: 10 * time.Millisecond,
			ReadFrame: func(ctx context.Context) ([]byte, error) {
				<-ctx.Done()
				readDone <- ctx.Err()
				return nil, ctx.Err()
			},
		},
		callActive: func() bool { return true },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rc.pumpVideoTrack(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()
	rc.outboundAudioReadyAt.Store(time.Now().Add(-qwenAudioBeforeVideoLead).UnixNano())
	rc.outboundAudioReady.Store(true)
	select {
	case err := <-readDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ReadFrame context error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ReadFrame did not receive its frame-interval deadline")
	}
}

func TestQwenAnswerRequiresAcceptedVideo(t *testing.T) {
	for name, fixture := range map[string]struct {
		answer string
		want   bool
	}{
		"recvonly": {"v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=recvonly\r\n", true},
		"sendrecv": {"v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=sendrecv\r\n", true},
		"missing":  {"v=0\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\n", false},
		"rejected": {"v=0\r\nm=video 0 UDP/TLS/RTP/SAVPF 102\r\na=inactive\r\n", false},
		"sendonly": {"v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 102\r\na=sendonly\r\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := qwenAnswerAcceptsVideo(fixture.answer); got != fixture.want {
				t.Fatalf("accepted = %t, want %t", got, fixture.want)
			}
		})
	}
}

func TestQwenVideoPrimesAudioBeforeFirstFrame(t *testing.T) {
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	track := &recordingSampleWriter{}
	rc := &RealtimeConn{audio: audio, sendTrack: track}
	started := time.Now()
	if err := rc.primeQwenAudioBeforeVideo(context.Background()); err != nil {
		t.Fatalf("prime Qwen audio: %v", err)
	}
	if !rc.outboundAudioReady.Load() {
		t.Fatal("audio primer did not open Qwen's media-order gate")
	}
	if got := track.writes.Load(); got != qwenAudioPrimerFrames {
		t.Fatalf("audio primer writes = %d, want %d", got, qwenAudioPrimerFrames)
	}
	if elapsed := time.Since(started); elapsed < qwenAudioBeforeVideoLead-20*time.Millisecond {
		t.Fatalf("audio primer duration = %v, want at least %v", elapsed, qwenAudioBeforeVideoLead)
	}
}

func TestQwenVideoPrimerReleasesAudioLockBetweenFrames(t *testing.T) {
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	firstWrite := make(chan struct{}, 1)
	rc := &RealtimeConn{
		audio: audio,
		sendTrack: &recordingSampleWriter{onWrite: func() {
			select {
			case firstWrite <- struct{}{}:
			default:
			}
		}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = rc.primeQwenAudioBeforeVideo(ctx)
	}()
	<-firstWrite
	lockAcquired := make(chan struct{})
	go func() {
		rc.outboundAudioMu.Lock()
		rc.outboundAudioMu.Unlock()
		close(lockAcquired)
	}()
	select {
	case <-lockAcquired:
	case <-time.After(60 * time.Millisecond):
		t.Fatal("audio primer held the outbound lock across paced frames")
	}
	cancel()
	<-done
}

func TestQwenVideoPrimerFailsClosedWithoutAudioPath(t *testing.T) {
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	for name, rc := range map[string]*RealtimeConn{
		"encoder": {sendTrack: &recordingSampleWriter{}},
		"track":   {audio: audio},
	} {
		t.Run(name, func(t *testing.T) {
			if err := rc.primeQwenAudioBeforeVideo(context.Background()); err == nil {
				t.Fatal("missing audio path must not report a successful primer")
			}
		})
	}
}

func TestQwenVideoSourceRejectsUnsupportedCodec(t *testing.T) {
	_, err := newPeerConnectionForProviderWithVideo(nil, ProviderQwen, &RealtimeVideoSource{
		Codec:     "jpeg",
		ReadFrame: func(context.Context) ([]byte, error) { return []byte{1}, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported realtime video codec") {
		t.Fatalf("unsupported codec error = %v", err)
	}
}

// TestSessionConfigWatcherAcksOnSessionUpdated: a clean session.updated closes the
// handshake, fires onConfigured exactly once, and wait returns nil.
func TestSessionConfigWatcherAcksOnSessionUpdated(t *testing.T) {
	var onCfg int
	w := newSessionConfigWatcher(func() { onCfg++ })
	w.observe([]byte(`{"type":"session.updated"}`))
	if err := w.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("wait after session.updated = %v, want nil", err)
	}
	if onCfg != 1 {
		t.Fatalf("onConfigured called %d times, want 1", onCfg)
	}
}

// TestSessionConfigWatcherFailsOnPreAckError pins S3: an error before the ack (a
// rejected session.update) must surface as a Connect error carrying the server
// reason, NOT wedge the call in "connecting".
func TestSessionConfigWatcherFailsOnPreAckError(t *testing.T) {
	w := newSessionConfigWatcher(nil)
	w.observe([]byte(`{"type":"error","error":{"code":"invalid_value","message":"unknown model"}}`))
	err := w.wait(context.Background(), time.Second)
	if err == nil {
		t.Fatal("rejected session.update must surface as a Connect error, not a wedge")
	}
	if !strings.Contains(err.Error(), "invalid_value") {
		t.Fatalf("error should carry the server code, got %v", err)
	}
}

// TestSessionConfigWatcherIgnoresErrorAfterAck: an error AFTER session.updated is a
// mid-call error, not a config failure — it must not turn a live session into one.
func TestSessionConfigWatcherIgnoresErrorAfterAck(t *testing.T) {
	w := newSessionConfigWatcher(nil)
	w.observe([]byte(`{"type":"session.updated"}`))
	w.observe([]byte(`{"type":"error","error":{"code":"mid_call"}}`))
	if err := w.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("post-ack error must not fail a configured session: %v", err)
	}
}

// TestSessionConfigWatcherTimesOut: a silent session.update (neither ack nor error)
// must time out rather than block forever.
func TestSessionConfigWatcherTimesOut(t *testing.T) {
	w := newSessionConfigWatcher(nil)
	err := w.wait(context.Background(), 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("silent session.update must time out, got %v", err)
	}
}

// TestSessionConfigWatcherCtxCancel: a cancelled ctx unblocks wait with ctx.Err().
func TestSessionConfigWatcherCtxCancel(t *testing.T) {
	w := newSessionConfigWatcher(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.wait(ctx, time.Second); err == nil {
		t.Fatal("cancelled ctx must unblock wait with an error")
	}
}

// TestSendTrackStatsSegmentsAndTotals pins the send-side accounting used to
// reconcile "gate passed N frames" with "track actually wrote M frames" — the
// counters that rule WriteSample/encode failures in or out when the server goes
// deaf mid-call (2026-07-02).
func TestSendTrackStatsSegmentsAndTotals(t *testing.T) {
	var s sendTrackStats
	s.beginSegment(10) // gate had already passed 10 frames before this segment
	s.noteWrite(nil)
	s.noteWrite(nil)
	s.noteWrite(errors.New("track closed"))
	s.noteEncodeErr()

	seg := s.segmentLine(14) // gate passed 4 more frames during the segment
	for _, want := range []string{"gate_passed=4", "written=2", "write_err=1", "encode_err=1"} {
		if !strings.Contains(seg, want) {
			t.Fatalf("segment line missing %q, got %q", want, seg)
		}
	}

	totals := s.totalsLine()
	for _, want := range []string{"written=2", "write_err=1", "encode_err=1"} {
		if !strings.Contains(totals, want) {
			t.Fatalf("totals line missing %q, got %q", want, totals)
		}
	}
}

func TestAssistantBoundaryClosesPriorUserMicSegment(t *testing.T) {
	gate := newVPIOMicNoiseGate()
	gate.open = true
	gate.hangover = gate.hangoverFrames
	gate.pending = append(gate.pending, make([]int16, audioFrameSize))

	if !resetMicGateAtAssistantBoundary(gate, false, true) {
		t.Fatal("assistant speech start did not reset the mic gate")
	}
	if gate.open || gate.hangover != 0 || len(gate.pending) != 0 {
		t.Fatalf("assistant speech start left prior user segment open: open=%v hangover=%d pending=%d", gate.open, gate.hangover, len(gate.pending))
	}
	if resetMicGateAtAssistantBoundary(gate, true, true) {
		t.Fatal("steady assistant speech reset the mic gate more than once")
	}
}

func TestQwenBargeProtectionDoesNotChangeOpenAIAdmission(t *testing.T) {
	if !qwenBargeProtectionEnabled(true, string(ProviderQwen)) {
		t.Fatal("Qwen VPIO call should enable playback echo admission")
	}
	if qwenBargeProtectionEnabled(true, string(ProviderOpenAI)) {
		t.Fatal("OpenAI VPIO call should retain its existing native-floor admission")
	}
	if qwenBargeProtectionEnabled(false, string(ProviderQwen)) {
		t.Fatal("non-VPIO Qwen call should not enable VPIO playback echo admission")
	}
}

func TestMintEphemeralRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dev-key" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Session struct {
				Type  string `json:"type"`
				Model string `json:"model"`
			} `json:"session"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Session.Type != "realtime" || !strings.Contains(body.Session.Model, "gpt-realtime") {
			t.Errorf("bad mint body: %+v", body.Session)
		}
		json.NewEncoder(w).Encode(map[string]any{"value": "ek_test123"})
	}))
	defer srv.Close()

	ek, err := mintEphemeralAt(context.Background(), srv.URL, "dev-key", "gpt-realtime-2.1-mini")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if ek != "ek_test123" {
		t.Errorf("ek = %q, want ek_test123", ek)
	}
}

func TestExchangeSDPRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer ephemeral-key" {
			t.Errorf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "offer-sdp" {
			t.Errorf("offer = %q", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("answer-sdp"))
	}))
	defer srv.Close()

	answer, err := exchangeSDP(context.Background(), srv.URL, "ephemeral-key", []byte("offer-sdp"))
	if err != nil {
		t.Fatalf("exchangeSDP: %v", err)
	}
	if answer != "answer-sdp" || calls.Load() != 1 {
		t.Fatalf("answer=%q calls=%d, want one successful create call", answer, calls.Load())
	}
}

func TestExchangeSDPDoesNotReplayFailedCreate(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("x-request-id", "req_test_503")
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := exchangeSDP(context.Background(), srv.URL, "ephemeral-key", []byte("offer-sdp"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") || !strings.Contains(err.Error(), "req_test_503") {
		t.Fatalf("exchangeSDP error=%v, want HTTP 503 with request ID", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want no replay for a failed create call", calls.Load())
	}
}
