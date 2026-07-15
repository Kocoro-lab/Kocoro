package koe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultPerceptionPollInterval = 100 * time.Millisecond
	defaultPerceptionHTTPTimeout  = 250 * time.Millisecond
	// YuNet inference on the CM4 advances its monotonic source timestamp at about
	// 2 Hz, with first-hand loaded gaps up to 1.247 s. Two seconds still fails
	// closed quickly on a stopped tracker without flapping during healthy inference.
	defaultFaceStaleTimeout    = 2 * time.Second
	maxPerceptionResponseBytes = 64 << 10
)

// DefaultPerceptionPollInterval is the product cadence for the robot-local
// snapshot stream. Exported so the resident runtime does not duplicate it.
func DefaultPerceptionPollInterval() time.Duration { return defaultPerceptionPollInterval }

// PerceptionHealth is the fail-closed aggregate health of one robot-local
// face/DOA poll. It is deliberately small: operator-facing details stay in Error,
// while the gaze gate only needs to distinguish healthy from unavailable input.
type PerceptionHealth string

const (
	PerceptionOK                PerceptionHealth = "ok"
	PerceptionFaceStale         PerceptionHealth = "face_stale"
	PerceptionDOAUnavailable    PerceptionHealth = "doa_unavailable"
	PerceptionDaemonUnreachable PerceptionHealth = "daemon_unreachable"
	PerceptionInvalidPayload    PerceptionHealth = "invalid_payload"
)

type FaceSample struct {
	Available bool
	Fresh     bool
	Detected  bool
	X         float64
	Y         float64
	Roll      float64
	SourceTS  float64
}

type DOASample struct {
	Available      bool
	Fresh          bool
	Angle          float64
	SpeechDetected bool
}

type PerceptionSnapshot struct {
	ObservedAt time.Time
	Face       FaceSample
	DOA        DOASample
	Health     PerceptionHealth
	Error      string
}

func (s PerceptionSnapshot) Healthy() bool {
	return s.Health == PerceptionOK && s.Face.Available && s.Face.Fresh && s.DOA.Available && s.DOA.Fresh
}

type PerceptionOption func(*PerceptionClient)

// WithPerceptionNow is a deterministic test seam for source-timestamp freshness.
func WithPerceptionNow(now func() time.Time) PerceptionOption {
	return func(c *PerceptionClient) { c.now = now }
}

// PerceptionClient converts daemon 1.9 snapshot GETs into one validated local
// time-series snapshot. Poll is serialized so a slow daemon can never create
// overlapping request storms.
type PerceptionClient struct {
	baseURL string
	http    *http.Client
	now     func() time.Time

	mu               sync.Mutex
	lastFaceSourceTS *float64
	lastFaceAdvanced time.Time
}

func NewPerceptionClient(rawBaseURL string, opts ...PerceptionOption) (*PerceptionClient, error) {
	u, err := url.Parse(rawBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("koe[perception]: invalid daemon URL %q", rawBaseURL)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("koe[perception]: daemon URL must not contain query or fragment")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("koe[perception]: daemon URL must not contain a path")
	}
	c := &PerceptionClient{
		baseURL: strings.TrimRight(rawBaseURL, "/"),
		http:    &http.Client{Timeout: defaultPerceptionHTTPTimeout},
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.now == nil {
		return nil, fmt.Errorf("koe[perception]: nil clock")
	}
	return c, nil
}

var (
	errPerceptionUnreachable = errors.New("perception daemon unreachable")
	errPerceptionInvalid     = errors.New("invalid perception payload")
)

type faceWire struct {
	Status     string          `json:"status"`
	FaceTarget *faceTargetWire `json:"face_target"`
}

type faceTargetWire struct {
	Detected *bool    `json:"detected"`
	X        *float64 `json:"x"`
	Y        *float64 `json:"y"`
	Roll     *float64 `json:"roll"`
	TS       *float64 `json:"ts"`
}

type doaWire struct {
	Angle          *float64 `json:"angle"`
	SpeechDetected *bool    `json:"speech_detected"`
}

func (c *PerceptionClient) Poll(ctx context.Context) PerceptionSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	s := PerceptionSnapshot{ObservedAt: now}
	face, faceErr := c.fetchFace(ctx)
	doa, doaErr := c.fetchDOA(ctx)
	s.Face = face
	s.DOA = doa

	if faceErr != nil || doaErr != nil {
		s.Error = joinPerceptionErrors(faceErr, doaErr)
		if errors.Is(faceErr, errPerceptionUnreachable) || errors.Is(doaErr, errPerceptionUnreachable) {
			s.Health = PerceptionDaemonUnreachable
		} else {
			s.Health = PerceptionInvalidPayload
		}
		return s
	}

	if face.Available {
		if c.lastFaceSourceTS == nil || face.SourceTS != *c.lastFaceSourceTS {
			ts := face.SourceTS
			c.lastFaceSourceTS = &ts
			c.lastFaceAdvanced = now
		}
		face.Fresh = !c.lastFaceAdvanced.IsZero() && now.Sub(c.lastFaceAdvanced) <= defaultFaceStaleTimeout
		s.Face = face
	}
	if !s.Face.Available || !s.Face.Fresh {
		s.Health = PerceptionFaceStale
		return s
	}
	if !s.DOA.Available {
		s.Health = PerceptionDOAUnavailable
		return s
	}
	s.Health = PerceptionOK
	return s
}

func joinPerceptionErrors(a, b error) string {
	parts := make([]string, 0, 2)
	if a != nil {
		parts = append(parts, a.Error())
	}
	if b != nil {
		parts = append(parts, b.Error())
	}
	return strings.Join(parts, "; ")
}

func (c *PerceptionClient) fetchFace(ctx context.Context) (FaceSample, error) {
	body, err := c.getJSON(ctx, "/api/media/tracking/face")
	if err != nil {
		return FaceSample{}, err
	}
	var wire faceWire
	if err := json.Unmarshal(body, &wire); err != nil || wire.Status != "ok" || wire.FaceTarget == nil || wire.FaceTarget.Detected == nil {
		return FaceSample{}, fmt.Errorf("%w: face response", errPerceptionInvalid)
	}
	if wire.FaceTarget.TS == nil {
		return FaceSample{}, nil // tracking disabled/not initialized: unavailable, not malformed
	}
	if !finite(*wire.FaceTarget.TS) || *wire.FaceTarget.TS < 0 {
		return FaceSample{}, fmt.Errorf("%w: face timestamp", errPerceptionInvalid)
	}
	s := FaceSample{Available: true, Detected: *wire.FaceTarget.Detected, SourceTS: *wire.FaceTarget.TS}
	if !s.Detected {
		return s, nil
	}
	if wire.FaceTarget.X == nil || wire.FaceTarget.Y == nil || wire.FaceTarget.Roll == nil {
		return FaceSample{}, fmt.Errorf("%w: detected face missing coordinates", errPerceptionInvalid)
	}
	if !finite(*wire.FaceTarget.X) || !finite(*wire.FaceTarget.Y) || !finite(*wire.FaceTarget.Roll) ||
		*wire.FaceTarget.X < -1 || *wire.FaceTarget.X > 1 || *wire.FaceTarget.Y < -1 || *wire.FaceTarget.Y > 1 {
		return FaceSample{}, fmt.Errorf("%w: face coordinates", errPerceptionInvalid)
	}
	s.X, s.Y, s.Roll = *wire.FaceTarget.X, *wire.FaceTarget.Y, *wire.FaceTarget.Roll
	return s, nil
}

func (c *PerceptionClient) fetchDOA(ctx context.Context) (DOASample, error) {
	body, err := c.getJSON(ctx, "/api/state/doa")
	if err != nil {
		return DOASample{}, err
	}
	var wire *doaWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return DOASample{}, fmt.Errorf("%w: doa response", errPerceptionInvalid)
	}
	if wire == nil {
		return DOASample{}, nil
	}
	if wire.Angle == nil || wire.SpeechDetected == nil || !finite(*wire.Angle) || *wire.Angle < 0 || *wire.Angle > math.Pi {
		return DOASample{}, fmt.Errorf("%w: doa angle", errPerceptionInvalid)
	}
	return DOASample{Available: true, Fresh: true, Angle: *wire.Angle, SpeechDetected: *wire.SpeechDetected}, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func (c *PerceptionClient) getJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request", errPerceptionInvalid)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errPerceptionUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s returned HTTP %d", errPerceptionUnreachable, path, resp.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, fmt.Errorf("%w: %s content type", errPerceptionInvalid, path)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPerceptionResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s body", errPerceptionUnreachable, path)
	}
	if len(body) > maxPerceptionResponseBytes {
		return nil, fmt.Errorf("%w: %s body exceeds %d bytes", errPerceptionInvalid, path, maxPerceptionResponseBytes)
	}
	return body, nil
}

// Stream polls immediately and then at interval, publishing a latest-only channel.
// The goroutine is single-owned, so a slow request can never overlap the next tick.
func (c *PerceptionClient) Stream(ctx context.Context, interval time.Duration) (<-chan PerceptionSnapshot, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("koe[perception]: poll interval must be positive")
	}
	out := make(chan PerceptionSnapshot, 1)
	go func() {
		defer close(out)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			publishLatest(out, c.Poll(ctx))
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out, nil
}

func publishLatest(ch chan PerceptionSnapshot, s PerceptionSnapshot) {
	select {
	case ch <- s:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- s:
	default:
	}
}
