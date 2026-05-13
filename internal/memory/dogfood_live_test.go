//go:build dogfood

// Run with (from the repo root):
//   go test -tags dogfood -v -timeout 180s -run TestDogfoodLive ./internal/memory/
//
// Requires a live Shannon Cloud endpoint configured in ~/.shannon/config.yaml
// (cloud.endpoint + cloud.api_key or memory.endpoint + memory.api_key). Spawns
// the real tlm sidecar, pulls the published bundle, and issues real Query
// calls against the sidecar UDS.

package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

type tAudit struct{ t *testing.T }

func (a tAudit) Log(event string, fields map[string]any) {
	b, _ := json.Marshal(fields)
	a.t.Logf("AUDIT %s %s %s", time.Now().UTC().Format(time.RFC3339Nano), event, string(b))
}

func TestDogfoodLive(t *testing.T) {
	if _, err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg := LoadConfig(viper.GetViper())
	cfg.APIKey = ResolveAPIKey(viper.GetViper())
	cfg.Endpoint = ResolveEndpoint(viper.GetViper())

	t.Logf("CFG provider=%s endpoint=%s socket=%s bundle_root=%s tlm_path=%q",
		cfg.Provider, cfg.Endpoint, cfg.SocketPath, cfg.BundleRoot, cfg.TLMPath)
	if cfg.Provider != "cloud" {
		t.Fatalf("memory.provider must be 'cloud' (got %q)", cfg.Provider)
	}
	if cfg.APIKey == "" {
		t.Fatal("no api_key resolved")
	}
	t.Logf("CFG api_key_len=%d key_prefix=%s", len(cfg.APIKey), cfg.APIKey[:8])

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	tStart := time.Now()

	// Service.Start now bootstraps the first bundle synchronously (the
	// cold-start fix); no pre-tick workaround needed.
	svc := NewService(cfg, tAudit{t})
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Service.Start: %v", err)
	}
	defer func() {
		dStop := time.Now()
		if err := svc.Stop(); err != nil {
			t.Logf("Stop err: %v", err)
		}
		t.Logf("T+%s SHUTDOWN duration=%s", dur(tStart), time.Since(dStop))
	}()
	t.Logf("T+%s START returned; status=%s", dur(tStart), svc.Status())

	waitReady(t, svc, tStart)
	waitBundle(t, cfg, tStart)

	// If the puller's first tick finds a newer ts on the server, give it
	// time to install and reload before we reconcile + query. Harmless if
	// we're already current; just polls the symlink for up to 30s.
	upgradeSettle(t, cfg, tStart)
	reconcileBundle(t, cfg)

	// Direct /bundle/reload probe — proves endpoint reachable + returns a
	// well-formed response. Expect swapped=false here because the bundle
	// bootstrap already pointed current→this ts; there's nothing newer to
	// swap to. A swapped=true response only occurs on a cross-ts upgrade.
	rcClient := NewClient(cfg.SocketPath, 5*time.Second)
	rcCtx, rcCancel := context.WithTimeout(ctx, 5*time.Second)
	t0 := time.Now()
	rc, rcErr := rcClient.Reload(rcCtx)
	rcCancel()
	if rcErr != nil {
		t.Logf("RELOAD err=%v latency=%s", rcErr, time.Since(t0))
	} else {
		cur := "<nil>"
		if rc.CurrentBundleDir != nil {
			cur = *rc.CurrentBundleDir
		}
		t.Logf("RELOAD swapped=%v trigger=%s reason=%s current_dir=%s duration_ms=%.2f latency=%s",
			rc.Swapped, rc.Trigger, rc.Reason, cur, rc.ReloadDurationMs, time.Since(t0))
	}

	// Warmup query — catches the lazy-load stall so the reported probe
	// latencies are hot-path figures.
	warmupCtx, warmupCancel := context.WithTimeout(ctx, 15*time.Second)
	warmStart := time.Now()
	_, _, warmErr := svc.Query(warmupCtx, QueryIntent{
		Mode: ModeDirectRelation, AnchorMentions: []string{"warmup"},
		ResultLimit: 1, EvidenceBudget: 1,
	})
	warmupCancel()
	t.Logf("WARMUP first_query_latency=%s err=%v", time.Since(warmStart), warmErr)

	queryProbe(t, ctx, svc)
}

func dur(t0 time.Time) string { return fmt.Sprintf("%.3fs", time.Since(t0).Seconds()) }

func waitReady(t *testing.T, svc *Service, tStart time.Time) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		s := svc.Status()
		if s == StatusReady {
			t.Logf("T+%s SIDECAR_READY", dur(tStart))
			return
		}
		if s == StatusDegraded || s == StatusUnavailable || s == StatusDisabled {
			t.Fatalf("unexpected service status: %s", s)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for Ready (last=%s)", svc.Status())
}

// upgradeSettle waits for the puller to finish any in-flight bundle
// upgrade. It watches the `current` symlink target; if it changes within
// the window we log both the old and new ts. Silent no-op when nothing
// upgrades (typical case: already current).
func upgradeSettle(t *testing.T, cfg Config, tStart time.Time) {
	link := filepath.Join(cfg.BundleRoot, "current")
	initial, _ := os.Readlink(link)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := os.Readlink(link)
		if err == nil && cur != initial && cur != "" {
			t.Logf("T+%s BUNDLE_UPGRADED old→%s  new→%s", dur(tStart),
				filepath.Base(initial), filepath.Base(cur))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func waitBundle(t *testing.T, cfg Config, tStart time.Time) {
	link := filepath.Join(cfg.BundleRoot, "current")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if target, err := os.Readlink(link); err == nil && target != "" {
			t.Logf("T+%s BUNDLE_INSTALLED current→%s", dur(tStart), target)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Logf("T+%s BUNDLE_NOT_INSTALLED (puller tick may have errored; check AUDIT)", dur(tStart))
}

type fileInfo struct {
	rel  string
	size int64
	sha  string
}

func reconcileBundle(t *testing.T, cfg Config) {
	link := filepath.Join(cfg.BundleRoot, "current")
	target, err := os.Readlink(link)
	if err != nil {
		t.Logf("RECONCILE no current symlink: %v", err)
		return
	}
	var files []fileInfo
	total := int64(0)
	err = filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		f, ferr := os.Open(p)
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		h := sha256.New()
		if _, cerr := io.Copy(h, f); cerr != nil {
			return cerr
		}
		rel, _ := filepath.Rel(target, p)
		files = append(files, fileInfo{rel, info.Size(), hex.EncodeToString(h.Sum(nil))})
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Logf("RECONCILE walk err: %v", err)
		return
	}
	t.Logf("RECONCILE bundle_dir=%s file_count=%d total_bytes=%d", target, len(files), total)
	for _, f := range files {
		short := f.sha
		if len(short) > 16 {
			short = short[:16] + "…"
		}
		t.Logf("  %-40s size=%d sha256=%s", f.rel, f.size, short)
	}
}

func queryProbe(t *testing.T, ctx context.Context, svc *Service) {
	prompts := []string{"user", "session", "browser", "hacker news", "sync"}
	for _, p := range prompts {
		intent := QueryIntent{
			Mode:           ModeDirectRelation,
			AnchorMentions: []string{p},
			ResultLimit:    5,
			EvidenceBudget: 5,
		}
		qCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		t0 := time.Now()
		env, class, err := svc.Query(qCtx, intent)
		lat := time.Since(t0)
		cancel()
		if err != nil {
			t.Logf("QUERY anchor=%q ERR %v latency=%s", p, err, lat)
			continue
		}
		if env == nil {
			t.Logf("QUERY anchor=%q nil envelope class=%d latency=%s", p, class, lat)
			continue
		}
		t.Logf("QUERY anchor=%q class=%d latency=%s cand_count=%d reason=%q bundle_v=%s",
			p, class, lat, len(env.Candidates), env.Reason, env.BundleVersion)
		for i, c := range env.Candidates {
			b, _ := json.Marshal(c)
			snippet := string(b)
			if len(snippet) > 240 {
				snippet = snippet[:240]
			}
			t.Logf("    cand[%d] score=%.4f evidence=%s: %s", i, c.Score, c.Evidence, snippet)
		}
		if len(env.Warnings) > 0 {
			t.Logf("    warnings=%+v", env.Warnings)
		}
	}
}
