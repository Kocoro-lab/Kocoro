//go:build darwin && cgo

package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/koe"
)

const (
	realtimeUsageRelayConcurrency    = 2
	realtimeUsageRelayRequestTimeout = 10 * time.Second
	realtimeUsageRelayDrainTimeout   = 10 * time.Second
	realtimeUsageRelayCancelGrace    = time.Second
	realtimeUsageRelayRetryBackoff   = 100 * time.Millisecond
	realtimeUsageRelayMaxBodyBytes   = 64 * 1024
	realtimeUsageRelayMaxSpoolBytes  = realtimeUsageRelayMaxBodyBytes + 4*1024
)

type realtimeUsageRelayItem struct {
	principal string
	body      json.RawMessage
	path      string
}

type realtimeUsageRelaySpoolEnvelope struct {
	Principal string          `json:"principal"`
	Usage     json.RawMessage `json:"usage"`
}

// realtimeUsageRelay owns the in-process handoff between Realtime callbacks and
// the daemon. Callback admission never waits for the daemon or worker capacity;
// it only completes the small local durable spool write. The worker pool is
// bounded, and Close drains accepted reports before cancelling the workers.
type realtimeUsageRelay struct {
	client    *koe.DaemonClient
	openAIKey string
	backlog   []realtimeUsageRelayItem
	ready     *sync.Cond
	done      chan struct{}
	cancel    context.CancelFunc
	spoolDir  string
	spoolErr  error

	mu        sync.Mutex
	spoolMu   sync.Mutex
	closed    bool
	closeOnce sync.Once
}

func newRealtimeUsageRelay(client *koe.DaemonClient, openAIKey string) *realtimeUsageRelay {
	spoolDir := ""
	if shannonDir := strings.TrimSpace(config.ShannonDir()); shannonDir != "" {
		spoolDir = filepath.Join(shannonDir, "realtime-usage-relay")
	}
	return newRealtimeUsageRelayWithSpool(client, openAIKey, spoolDir)
}

func newRealtimeUsageRelayWithSpool(client *koe.DaemonClient, openAIKey, spoolDir string) *realtimeUsageRelay {
	relay := &realtimeUsageRelay{
		client:    client,
		openAIKey: openAIKey,
		done:      make(chan struct{}),
		spoolDir:  spoolDir,
	}
	relay.ready = sync.NewCond(&relay.mu)
	if client == nil {
		close(relay.done)
		return relay
	}
	relay.backlog, relay.spoolErr = relay.loadSpool()

	workerCtx, cancel := context.WithCancel(context.Background())
	relay.cancel = cancel
	var workers sync.WaitGroup
	workers.Add(realtimeUsageRelayConcurrency)
	for i := 0; i < realtimeUsageRelayConcurrency; i++ {
		go func() {
			defer workers.Done()
			relay.run(workerCtx)
		}()
	}
	go func() {
		workers.Wait()
		close(relay.done)
	}()
	return relay
}

// Enqueue is safe to call concurrently from provider event callbacks. It never
// waits for the daemon or for worker capacity. The report is installed in the
// private spool before it is admitted to the in-memory backlog, so a process
// exit cannot lose an accepted response.done event.
func (r *realtimeUsageRelay) Enqueue(principal string, usage json.RawMessage) error {
	if r == nil || r.client == nil || !shouldRelayRealtimeUsage(r.openAIKey, usage) {
		return nil
	}
	if len(usage) == 0 || len(usage) > realtimeUsageRelayMaxBodyBytes {
		return fmt.Errorf("realtime usage relay body exceeds limit")
	}
	if _, err := realtimeUsageRelayResponseID(usage); err != nil {
		return fmt.Errorf("invalid realtime usage relay body: %w", err)
	}
	item := realtimeUsageRelayItem{
		principal: principal,
		body:      append(json.RawMessage(nil), usage...),
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fmt.Errorf("realtime usage relay is closed")
	}
	if r.spoolErr != nil {
		err := r.spoolErr
		r.mu.Unlock()
		return fmt.Errorf("realtime usage relay spool unavailable: %w", err)
	}
	path, created, err := r.persistSpool(principal, item.body)
	if err != nil {
		// persistSpool may have installed the target before a directory fsync
		// error. Keep that report queued even though admission is reported to the
		// caller, preserving the no-drop guarantee.
		if path != "" && created {
			item.path = path
			r.backlog = append(r.backlog, item)
			r.ready.Signal()
		}
		r.mu.Unlock()
		return err
	}
	if !created {
		r.mu.Unlock()
		return nil
	}
	item.path = path
	r.backlog = append(r.backlog, item)
	r.ready.Signal()
	r.mu.Unlock()
	return nil
}

func (r *realtimeUsageRelay) run(ctx context.Context) {
	for {
		r.mu.Lock()
		for len(r.backlog) == 0 && !r.closed {
			r.ready.Wait()
		}
		if len(r.backlog) == 0 && r.closed {
			r.mu.Unlock()
			return
		}
		item := r.backlog[0]
		r.backlog[0] = realtimeUsageRelayItem{}
		r.backlog = r.backlog[1:]
		r.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		reqCtx, cancel := context.WithTimeout(ctx, realtimeUsageRelayRequestTimeout)
		err := r.client.SendRealtimeUsageWithPrincipal(reqCtx, item.body, item.principal)
		cancel()
		if err == nil {
			if removeErr := r.removeSpool(item.path); removeErr != nil {
				err = removeErr
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("koe: usage relay failed: %v", err)
			if !r.retry(item, ctx) {
				return
			}
		}
	}
}

func (r *realtimeUsageRelay) retry(item realtimeUsageRelayItem, ctx context.Context) bool {
	timer := time.NewTimer(realtimeUsageRelayRetryBackoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	}
	if ctx.Err() != nil {
		return false
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	r.backlog = append(r.backlog, item)
	r.ready.Signal()
	r.mu.Unlock()
	return true
}

func (r *realtimeUsageRelay) persistSpool(principal string, body json.RawMessage) (string, bool, error) {
	r.spoolMu.Lock()
	defer r.spoolMu.Unlock()
	return persistRealtimeUsageRelaySpool(r.spoolDir, principal, body)
}

func (r *realtimeUsageRelay) removeSpool(path string) error {
	if path == "" {
		return nil
	}
	r.spoolMu.Lock()
	defer r.spoolMu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove realtime usage relay spool: %w", err)
	}
	return syncDirectory(r.spoolDir)
}

func realtimeUsageRelayResponseID(body []byte) (string, error) {
	_, responseID, err := realtimeUsageRelayIdentity(body)
	return responseID, err
}

func realtimeUsageRelayIdentity(body []byte) (string, string, error) {
	var payload struct {
		Provider   string `json:"provider"`
		ResponseID string `json:"response_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", fmt.Errorf("decode usage body: %w", err)
	}
	provider := strings.ToLower(strings.TrimSpace(payload.Provider))
	if provider == "" {
		return "", "", fmt.Errorf("usage provider is required")
	}
	responseID := strings.TrimSpace(payload.ResponseID)
	if responseID == "" {
		return "", "", fmt.Errorf("usage response_id is required")
	}
	return provider, responseID, nil
}

func realtimeUsageRelayTarget(spoolDir, principal, provider, responseID string) string {
	identity := strings.TrimSpace(principal) + "\x00" + strings.ToLower(strings.TrimSpace(provider)) + "\x00" + strings.TrimSpace(responseID)
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(spoolDir, "usage-"+hex.EncodeToString(sum[:])+".json")
}

func realtimeUsageRelayLegacyTarget(spoolDir, responseID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(responseID)))
	return filepath.Join(spoolDir, "usage-"+hex.EncodeToString(sum[:])+".json")
}

// installRealtimeUsageRelayFile links the fsynced temp inode into place. Unlike
// os.Rename, os.Link never replaces an existing target, including when another
// Koe process is using the same Shannon directory during restart.
func installRealtimeUsageRelayFile(tmpPath, target string) (bool, error) {
	if err := os.Link(tmpPath, target); err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := os.Remove(tmpPath); err != nil {
		return false, err
	}
	return true, nil
}

func ensureRealtimeUsageRelayDir(spoolDir string) error {
	if strings.TrimSpace(spoolDir) == "" {
		return fmt.Errorf("realtime usage relay spool is not configured")
	}
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		return fmt.Errorf("create realtime usage relay spool: %w", err)
	}
	if err := os.Chmod(spoolDir, 0o700); err != nil {
		return fmt.Errorf("protect realtime usage relay spool: %w", err)
	}
	return nil
}

func realtimeUsageRelayTargetIsValid(spoolDir, target string) (bool, error) {
	if _, err := os.Stat(target); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	raw, err := readRealtimeUsageRelayFile(target)
	if err == nil {
		envelope, provider, responseID, decodeErr := decodeRealtimeUsageRelayEnvelope(raw)
		if decodeErr == nil && realtimeUsageRelayTarget(spoolDir, envelope.Principal, provider, responseID) == target {
			return true, nil
		}
	}
	if err := os.Rename(target, target+".invalid"); err != nil {
		return false, fmt.Errorf("isolate invalid realtime usage relay spool: %w", err)
	}
	return false, nil
}

func realtimeUsageRelayLegacyTargetIsValid(spoolDir, target string) (bool, error) {
	if _, err := os.Stat(target); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	raw, err := readRealtimeUsageRelayFile(target)
	if err == nil {
		_, _, responseID, decodeErr := decodeRealtimeUsageRelayEnvelope(raw)
		if decodeErr == nil && realtimeUsageRelayLegacyTarget(spoolDir, responseID) == target {
			return true, nil
		}
	}
	return false, nil
}

func realtimeUsageRelayCanonicalMatches(spoolDir, target, principal, provider, responseID string) (bool, error) {
	raw, err := readRealtimeUsageRelayFile(target)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	envelope, storedProvider, storedResponseID, err := decodeRealtimeUsageRelayEnvelope(raw)
	if err != nil {
		return false, fmt.Errorf("decode canonical realtime usage relay spool: %w", err)
	}
	if strings.TrimSpace(envelope.Principal) != strings.TrimSpace(principal) ||
		storedProvider != strings.ToLower(strings.TrimSpace(provider)) ||
		storedResponseID != strings.TrimSpace(responseID) ||
		realtimeUsageRelayTarget(spoolDir, envelope.Principal, storedProvider, storedResponseID) != target {
		return false, fmt.Errorf("canonical realtime usage relay spool identity mismatch")
	}
	return true, nil
}

func migrateRealtimeUsageRelayLegacyFile(spoolDir, legacyTarget string) (string, bool, error) {
	valid, err := realtimeUsageRelayLegacyTargetIsValid(spoolDir, legacyTarget)
	if err != nil || !valid {
		return "", false, err
	}
	raw, err := readRealtimeUsageRelayFile(legacyTarget)
	if err != nil {
		return "", false, err
	}
	envelope, provider, responseID, err := decodeRealtimeUsageRelayEnvelope(raw)
	if err != nil {
		return "", false, err
	}
	canonical := realtimeUsageRelayTarget(spoolDir, envelope.Principal, provider, responseID)
	if canonical == legacyTarget {
		return canonical, true, nil
	}
	_, err = installRealtimeUsageRelayFile(legacyTarget, canonical)
	if err != nil {
		return "", false, err
	}
	matched, err := realtimeUsageRelayCanonicalMatches(spoolDir, canonical, envelope.Principal, provider, responseID)
	if err != nil {
		return "", false, err
	}
	if !matched {
		return "", false, fmt.Errorf("canonical realtime usage relay spool disappeared during migration")
	}
	if err := os.Remove(legacyTarget); err != nil && !os.IsNotExist(err) {
		return canonical, true, err
	}
	if err := syncDirectory(spoolDir); err != nil {
		return canonical, true, err
	}
	return canonical, true, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func persistRealtimeUsageRelaySpool(spoolDir, principal string, body json.RawMessage) (path string, created bool, err error) {
	if err := ensureRealtimeUsageRelayDir(spoolDir); err != nil {
		return "", false, err
	}
	provider, responseID, err := realtimeUsageRelayIdentity(body)
	if err != nil {
		return "", false, err
	}
	target := realtimeUsageRelayTarget(spoolDir, principal, provider, responseID)
	legacyTarget := realtimeUsageRelayLegacyTarget(spoolDir, responseID)
	if legacyTarget != target {
		legacyCanonical, legacyExists, legacyErr := migrateRealtimeUsageRelayLegacyFile(spoolDir, legacyTarget)
		if legacyErr != nil {
			return "", false, fmt.Errorf("migrate legacy realtime usage relay spool: %w", legacyErr)
		}
		if legacyExists && legacyCanonical == target {
			return target, false, nil
		}
	}
	valid, err := realtimeUsageRelayTargetIsValid(spoolDir, target)
	if err != nil {
		return "", false, fmt.Errorf("check realtime usage relay spool: %w", err)
	}
	if valid {
		return target, false, nil
	}

	envelope, err := json.Marshal(realtimeUsageRelaySpoolEnvelope{
		Principal: strings.TrimSpace(principal),
		Usage:     append(json.RawMessage(nil), body...),
	})
	if err != nil {
		return "", false, fmt.Errorf("encode realtime usage relay spool: %w", err)
	}
	tmp, err := os.CreateTemp(spoolDir, ".pending-*")
	if err != nil {
		return "", false, fmt.Errorf("create realtime usage relay temp: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		_ = tmp.Close()
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", false, fmt.Errorf("protect realtime usage relay temp: %w", err)
	}
	if _, err := tmp.Write(envelope); err != nil {
		return "", false, fmt.Errorf("write realtime usage relay temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return "", false, fmt.Errorf("sync realtime usage relay temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", false, fmt.Errorf("close realtime usage relay temp: %w", err)
	}
	valid, err = realtimeUsageRelayTargetIsValid(spoolDir, target)
	if err != nil {
		return "", false, fmt.Errorf("check realtime usage relay spool: %w", err)
	}
	if valid {
		return target, false, nil
	}
	installed, installErr := installRealtimeUsageRelayFile(tmpPath, target)
	if installErr != nil {
		return "", false, fmt.Errorf("install realtime usage relay spool: %w", installErr)
	}
	if !installed {
		matched, verifyErr := realtimeUsageRelayCanonicalMatches(spoolDir, target, principal, provider, responseID)
		if verifyErr != nil {
			return "", false, fmt.Errorf("verify existing realtime usage relay spool: %w", verifyErr)
		}
		if !matched {
			return "", false, fmt.Errorf("existing realtime usage relay spool disappeared during install")
		}
		return target, false, nil
	}
	removeTemp = false
	if err := syncDirectory(spoolDir); err != nil {
		// The target is installed and remains replayable. Surface the fsync error
		// to the caller while retaining the report in the lossless queue.
		return target, true, fmt.Errorf("sync realtime usage relay spool: %w", err)
	}
	return target, true, nil
}

func decodeRealtimeUsageRelayEnvelope(raw []byte) (realtimeUsageRelaySpoolEnvelope, string, string, error) {
	var envelope realtimeUsageRelaySpoolEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return envelope, "", "", err
	}
	if len(envelope.Usage) == 0 || len(envelope.Usage) > realtimeUsageRelayMaxBodyBytes || !json.Valid(envelope.Usage) {
		return envelope, "", "", fmt.Errorf("invalid usage payload")
	}
	provider, responseID, err := realtimeUsageRelayIdentity(envelope.Usage)
	if err != nil {
		return envelope, "", "", err
	}
	return envelope, provider, responseID, nil
}

func readRealtimeUsageRelayFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, realtimeUsageRelayMaxSpoolBytes+1))
}

func (r *realtimeUsageRelay) loadSpool() ([]realtimeUsageRelayItem, error) {
	if err := ensureRealtimeUsageRelayDir(r.spoolDir); err != nil {
		return nil, err
	}
	if err := r.recoverPendingSpool(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(r.spoolDir)
	if err != nil {
		return nil, fmt.Errorf("read realtime usage relay spool: %w", err)
	}
	items := make([]realtimeUsageRelayItem, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "usage-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(r.spoolDir, entry.Name())
		if err := os.Chmod(path, 0o600); err != nil {
			continue
		}
		raw, readErr := readRealtimeUsageRelayFile(path)
		envelope, provider, responseID, decodeErr := decodeRealtimeUsageRelayEnvelope(raw)
		if readErr != nil || decodeErr != nil {
			// Canonical corruption is isolated from the active queue. It is kept
			// private for diagnosis but cannot repeatedly block valid reports.
			_ = os.Rename(path, path+".invalid")
			continue
		}
		canonical := realtimeUsageRelayTarget(r.spoolDir, envelope.Principal, provider, responseID)
		if canonical != path {
			legacy := realtimeUsageRelayLegacyTarget(r.spoolDir, responseID)
			if legacy != path {
				_ = os.Rename(path, path+".invalid")
				continue
			}
			_, installErr := installRealtimeUsageRelayFile(path, canonical)
			if installErr != nil {
				return nil, fmt.Errorf("migrate realtime usage relay spool: %w", installErr)
			}
			matched, matchErr := realtimeUsageRelayCanonicalMatches(r.spoolDir, canonical, envelope.Principal, provider, responseID)
			if matchErr != nil {
				return nil, fmt.Errorf("check migrated realtime usage relay spool: %w", matchErr)
			}
			if !matched {
				return nil, fmt.Errorf("migrated realtime usage relay spool disappeared")
			}
			if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
				return nil, fmt.Errorf("remove legacy realtime usage relay spool: %w", removeErr)
			}
			if syncErr := syncDirectory(r.spoolDir); syncErr != nil {
				return nil, fmt.Errorf("sync migrated realtime usage relay spool: %w", syncErr)
			}
			path = canonical
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		items = append(items, realtimeUsageRelayItem{
			principal: envelope.Principal,
			body:      append(json.RawMessage(nil), envelope.Usage...),
			path:      path,
		})
	}
	return items, nil
}

func (r *realtimeUsageRelay) recoverPendingSpool() error {
	entries, err := os.ReadDir(r.spoolDir)
	if err != nil {
		return fmt.Errorf("read realtime usage relay pending files: %w", err)
	}
	changed := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".pending-") {
			continue
		}
		pendingPath := filepath.Join(r.spoolDir, entry.Name())
		if chmodErr := os.Chmod(pendingPath, 0o600); chmodErr != nil {
			return fmt.Errorf("protect realtime usage relay pending file: %w", chmodErr)
		}
		raw, readErr := readRealtimeUsageRelayFile(pendingPath)
		envelope, provider, responseID, decodeErr := decodeRealtimeUsageRelayEnvelope(raw)
		if readErr != nil || decodeErr != nil {
			// A crash may leave a partial temp. Quarantine it rather than retrying
			// the same invalid file on every startup.
			_ = os.Rename(pendingPath, pendingPath+".invalid")
			changed = true
			continue
		}
		target := realtimeUsageRelayTarget(r.spoolDir, envelope.Principal, provider, responseID)
		validTarget, targetErr := realtimeUsageRelayTargetIsValid(r.spoolDir, target)
		if targetErr != nil {
			return fmt.Errorf("check realtime usage relay pending target: %w", targetErr)
		}
		if validTarget {
			_ = os.Remove(pendingPath)
			changed = true
			continue
		}
		if err := os.Chmod(pendingPath, 0o600); err != nil {
			return fmt.Errorf("protect realtime usage relay pending file: %w", err)
		}
		// Re-sync a complete pending payload before installing it. This closes
		// the fsync/rename crash window without ever overwriting a canonical file.
		file, openErr := os.OpenFile(pendingPath, os.O_RDWR, 0o600)
		if openErr != nil {
			return fmt.Errorf("open realtime usage relay pending file: %w", openErr)
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return fmt.Errorf("sync realtime usage relay pending file: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close realtime usage relay pending file: %w", closeErr)
		}
		installed, installErr := installRealtimeUsageRelayFile(pendingPath, target)
		if installErr != nil {
			return fmt.Errorf("recover realtime usage relay pending file: %w", installErr)
		}
		if !installed {
			if err := os.Remove(pendingPath); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove duplicate realtime usage relay pending file: %w", err)
			}
		}
		changed = true
	}
	if changed {
		return syncDirectory(r.spoolDir)
	}
	return nil
}

// Close stops admission, drains accepted reports for a bounded interval, and
// then cancels any request that is still in flight. It is idempotent.
func (r *realtimeUsageRelay) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.ready.Broadcast()
		r.mu.Unlock()
	})

	timer := time.NewTimer(realtimeUsageRelayDrainTimeout)
	defer timer.Stop()
	select {
	case <-r.done:
		if r.cancel != nil {
			r.cancel()
		}
		return
	case <-timer.C:
		r.mu.Lock()
		pending := len(r.backlog)
		r.mu.Unlock()
		log.Printf("koe: realtime usage relay drain timed out with %d reports pending", pending)
		if r.cancel != nil {
			r.cancel()
		}
	}

	// HTTP requests honor their context. Keep Close bounded even if a custom
	// transport fails to return promptly after cancellation.
	grace := time.NewTimer(realtimeUsageRelayCancelGrace)
	defer grace.Stop()
	select {
	case <-r.done:
	case <-grace.C:
	}
}
