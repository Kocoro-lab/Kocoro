package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const (
	realtimeUsageOutboxName     = "realtime-usage-outbox"
	realtimeUsageOutboxInterval = 5 * time.Second
	// Usage payloads are small and should not hold the credential-generation
	// lease for the gateway client's long-lived default HTTP timeout.
	realtimeUsageSendTimeout  = 10 * time.Second
	realtimeUsageMaxBodyBytes = 64 << 10
)

// realtimeUsageOutbox is a private, file-backed relay queue. The provider,
// response ID, and principal form the durable identity: a repeated report while
// the first one is pending never overwrites the original payload or creates a
// second file, while equal response IDs from different providers remain distinct.
type realtimeUsageOutbox struct {
	dir      string
	mu       sync.Mutex
	inFlight map[string]struct{}
}

func (s *Server) realtimeUsageOutboxStore() *realtimeUsageOutbox {
	if s == nil || s.deps == nil || strings.TrimSpace(s.deps.ShannonDir) == "" {
		return nil
	}
	s.realtimeUsageOutboxMu.Lock()
	defer s.realtimeUsageOutboxMu.Unlock()
	if s.realtimeUsageOutbox == nil {
		s.realtimeUsageOutbox = newRealtimeUsageOutbox(s.deps.ShannonDir)
	}
	return s.realtimeUsageOutbox
}

func newRealtimeUsageOutbox(shannonDir string) *realtimeUsageOutbox {
	shannonDir = strings.TrimSpace(shannonDir)
	if shannonDir == "" {
		return nil
	}
	return &realtimeUsageOutbox{
		dir:      filepath.Join(shannonDir, realtimeUsageOutboxName),
		inFlight: make(map[string]struct{}),
	}
}

// realtimeUsagePrincipalFingerprint binds a report to a stable Cloud account
// without persisting or exposing the account identifier or API key.
func realtimeUsagePrincipalFingerprint(kind, value string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + value))
	return hex.EncodeToString(h[:])
}

func validRealtimeUsagePrincipal(principal string) bool {
	principal = strings.TrimSpace(principal)
	if len(principal) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(principal)
	return err == nil
}

// realtimeUsagePrincipal returns the account currently authorized to use the
// gateway. Auth-managed daemons require a verified account ID; legacy daemons
// use an endpoint+key fingerprint because they have no account API.
func (s *Server) realtimeUsagePrincipal() (string, bool) {
	if s == nil || s.deps == nil {
		return "", false
	}
	if s.auth != nil {
		accountID, ok := s.auth.VerifiedAccountID()
		if !ok {
			return "", false
		}
		if s.deps.GW == nil {
			return "", false
		}
		boundAccount, bound := s.deps.GW.IntegrationPrincipal()
		if !bound || boundAccount != accountID {
			return "", false
		}
		return realtimeUsagePrincipalFingerprint("account", accountID), true
	}
	cfg, _, _ := s.deps.Snapshot()
	if cfg == nil {
		return "", false
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	key := strings.TrimSpace(cfg.APIKey)
	if s.deps.GW != nil {
		// The legacy client is process-scoped and its GatewayClient credential
		// is immutable until restart. Use that live pair instead of a freshly
		// reloaded yaml value that the client has not adopted yet.
		endpoint = strings.TrimSpace(s.deps.GW.BaseURL())
		key = strings.TrimSpace(s.deps.GW.APIKey())
	}
	if endpoint == "" || key == "" {
		return "", false
	}
	return realtimeUsagePrincipalFingerprint("legacy", strings.TrimRight(endpoint, "/")+"\x00"+key), true
}

// withRealtimeUsageGatewayLease binds the bootstrap response to the exact
// gateway credential/principal generation that performed the Cloud request.
// The lease is intentionally held only around the request; it prevents an
// account/key mutation from changing the credential between identity capture
// and dispatch. It also avoids taking AuthManager's mutex while the gateway
// mutation path may be waiting for this lease.
func (s *Server) withRealtimeUsageGatewayLease(
	gw *client.GatewayClient,
	fn func(string) (json.RawMessage, error),
) (json.RawMessage, string, error) {
	if gw == nil || fn == nil {
		return nil, "", errors.New("realtime usage gateway is not configured")
	}
	generation, active := gw.IntegrationGeneration()
	if !active {
		return nil, "", errors.New("realtime usage principal unavailable")
	}
	var (
		body      json.RawMessage
		callErr   error
		principal string
	)
	leaseErr := gw.WithIntegrationGeneration(generation, func() {
		var ok bool
		principal, ok = s.realtimeUsagePrincipalForGatewayLease(gw)
		if !ok {
			callErr = errors.New("realtime usage principal unavailable")
			return
		}
		body, callErr = fn(principal)
	})
	if leaseErr != nil {
		return nil, "", leaseErr
	}
	return body, principal, callErr
}

// realtimeUsagePrincipalForGatewayLease reads only gateway/config state. It
// must be called while the GatewayClient integration dispatch lease is held;
// avoiding AuthManager's mutex here prevents account mutation deadlocks.
func (s *Server) realtimeUsagePrincipalForGatewayLease(gw *client.GatewayClient) (string, bool) {
	if s == nil || s.deps == nil || gw == nil {
		return "", false
	}
	if s.auth != nil {
		accountID, bound := gw.IntegrationPrincipal()
		if !bound || accountID == "" {
			return "", false
		}
		return realtimeUsagePrincipalFingerprint("account", accountID), true
	}
	cfg, _, _ := s.deps.Snapshot()
	key := gw.APIKey()
	endpoint := ""
	if gw != nil {
		endpoint = strings.TrimSpace(gw.BaseURL())
	}
	if endpoint == "" && cfg != nil {
		endpoint = strings.TrimSpace(cfg.Endpoint)
	}
	if endpoint == "" || strings.TrimSpace(key) == "" {
		return "", false
	}
	return realtimeUsagePrincipalFingerprint("legacy", strings.TrimRight(endpoint, "/")+"\x00"+key), true
}

func (s *Server) sendRealtimeUsageWithGatewayLease(ctx context.Context, gw *client.GatewayClient, principal string, body json.RawMessage) error {
	if gw == nil || !validRealtimeUsagePrincipal(principal) {
		return errors.New("realtime usage principal unavailable")
	}
	// Start the short usage deadline before taking the integration lease. A
	// stalled usage POST must release the lease promptly so sign-out or key
	// rotation is never held behind GatewayClient's 600-second default timeout.
	sendCtx, cancel := context.WithTimeout(ctx, realtimeUsageSendTimeout)
	defer cancel()
	generation, active := gw.IntegrationGeneration()
	if !active {
		return errors.New("realtime usage principal unavailable")
	}
	var sendErr error
	leaseErr := gw.WithIntegrationGeneration(generation, func() {
		current, ok := s.realtimeUsagePrincipalForGatewayLease(gw)
		if !ok || current != principal {
			sendErr = errors.New("realtime usage principal changed")
			return
		}
		if err := sendCtx.Err(); err != nil {
			sendErr = err
			return
		}
		_, sendErr = gw.SendRealtimeUsage(sendCtx, body)
	})
	if leaseErr != nil {
		return leaseErr
	}
	return sendErr
}

func addRealtimeUsagePrincipal(raw []byte, principal string) ([]byte, error) {
	if strings.TrimSpace(principal) == "" {
		return nil, errors.New("realtime usage principal is required")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("decode realtime bootstrap response: %w", err)
	}
	if body == nil {
		return nil, errors.New("realtime bootstrap response must be an object")
	}
	body["usage_principal"] = principal
	return json.Marshal(body)
}

func (o *realtimeUsageOutbox) ensureDir() error {
	if o == nil || o.dir == "" {
		return errors.New("realtime usage outbox is not configured")
	}
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return fmt.Errorf("create realtime usage outbox: %w", err)
	}
	// MkdirAll preserves an existing directory's mode. Tighten it on every
	// access because the usage payload must remain private even if an operator
	// created the directory with a broader mode before the daemon started.
	if err := os.Chmod(o.dir, 0o700); err != nil {
		return fmt.Errorf("protect realtime usage outbox: %w", err)
	}
	return nil
}

func (o *realtimeUsageOutbox) ensurePrincipalDir(principal string) error {
	if !validRealtimeUsagePrincipal(principal) {
		return errors.New("realtime usage principal is required")
	}
	if err := o.ensureDir(); err != nil {
		return err
	}
	dir := filepath.Join(o.dir, principal)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create realtime usage principal outbox: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect realtime usage principal outbox: %w", err)
	}
	return nil
}

func realtimeUsageIdentity(body []byte) (provider, responseID string, err error) {
	var payload struct {
		Provider   string          `json:"provider"`
		ResponseID string          `json:"response_id"`
		Usage      json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", fmt.Errorf("decode realtime usage: %w", err)
	}
	provider = strings.ToLower(strings.TrimSpace(payload.Provider))
	if provider == "" {
		return "", "", errors.New("realtime usage provider is required")
	}
	responseID = strings.TrimSpace(payload.ResponseID)
	if responseID == "" {
		return "", "", errors.New("realtime usage response_id is required")
	}
	if len(payload.Usage) == 0 || string(payload.Usage) == "null" {
		return "", "", errors.New("realtime usage usage is required")
	}
	return provider, responseID, nil
}

func realtimeUsageResponseID(body []byte) (string, error) {
	_, responseID, err := realtimeUsageIdentity(body)
	return responseID, err
}

func (o *realtimeUsageOutbox) targetPath(principal, provider, responseID string) string {
	identity := strings.TrimSpace(principal) + "\x00" + strings.ToLower(strings.TrimSpace(provider)) + "\x00" + strings.TrimSpace(responseID)
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(o.dir, principal, hex.EncodeToString(sum[:])+".json")
}

func (o *realtimeUsageOutbox) legacyTargetPath(principal, responseID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(responseID)))
	return filepath.Join(o.dir, principal, hex.EncodeToString(sum[:])+".json")
}

// installRealtimeUsageOutboxFile uses a hard link as an atomic no-replace
// install. os.Rename would overwrite an existing canonical file on POSIX,
// which could lose a different provider's report during recovery.
func installRealtimeUsageOutboxFile(tmpPath, target string) (bool, error) {
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

func (o *realtimeUsageOutbox) canonicalEntryMatches(canonical, principal, provider, responseID string) (bool, error) {
	body, err := os.ReadFile(canonical)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	storedProvider, storedResponseID, err := realtimeUsageIdentity(body)
	if err != nil {
		return false, fmt.Errorf("decode canonical realtime usage outbox entry: %w", err)
	}
	if storedProvider != strings.ToLower(strings.TrimSpace(provider)) ||
		storedResponseID != strings.TrimSpace(responseID) ||
		o.targetPath(principal, storedProvider, storedResponseID) != canonical {
		return false, fmt.Errorf("canonical realtime usage outbox entry identity mismatch")
	}
	return true, nil
}

func (o *realtimeUsageOutbox) migrateLegacyFile(principal, responseID string) (string, bool, error) {
	legacyTarget := o.legacyTargetPath(principal, responseID)
	if _, err := os.Stat(legacyTarget); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	body, err := os.ReadFile(legacyTarget)
	if err != nil {
		return "", false, err
	}
	provider, storedID, err := realtimeUsageIdentity(body)
	if err != nil || storedID != responseID {
		if renameErr := os.Rename(legacyTarget, legacyTarget+".invalid"); renameErr != nil && !os.IsNotExist(renameErr) {
			return "", false, renameErr
		}
		// A pre-provider or corrupt legacy entry cannot be charged safely, but it
		// must not block a new valid report that happens to reuse its response ID.
		return "", false, nil
	}
	canonical := o.targetPath(principal, provider, storedID)
	if canonical == legacyTarget {
		return canonical, true, nil
	}
	_, err = installRealtimeUsageOutboxFile(legacyTarget, canonical)
	if err != nil {
		return "", false, err
	}
	matched, err := o.canonicalEntryMatches(canonical, principal, provider, storedID)
	if err != nil {
		return "", false, err
	}
	if !matched {
		return "", false, errors.New("canonical realtime usage outbox entry disappeared during migration")
	}
	if err := os.Remove(legacyTarget); err != nil && !os.IsNotExist(err) {
		return canonical, true, err
	}
	if err := syncDirectory(filepath.Dir(canonical)); err != nil {
		return canonical, true, err
	}
	return canonical, true, nil
}

func (o *realtimeUsageOutbox) migrateLegacyEntriesLocked(principal string) error {
	entries, err := os.ReadDir(filepath.Join(o.dir, principal))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(o.dir, principal, entry.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		provider, responseID, err := realtimeUsageIdentity(body)
		if err != nil {
			continue
		}
		if o.targetPath(principal, provider, responseID) == path || o.legacyTargetPath(principal, responseID) != path {
			continue
		}
		if _, _, err := o.migrateLegacyFile(principal, responseID); err != nil {
			return fmt.Errorf("migrate legacy realtime usage outbox entry: %w", err)
		}
	}
	return nil
}

// enqueue durably installs body before any Cloud relay. It returns false
// when the response ID was already pending, which lets the HTTP handler avoid
// re-sending a duplicate report.
func (o *realtimeUsageOutbox) enqueue(body []byte, principal string) (created bool, err error) {
	if len(body) == 0 || len(body) > realtimeUsageMaxBodyBytes {
		return false, fmt.Errorf("realtime usage body must be between 1 and %d bytes", realtimeUsageMaxBodyBytes)
	}
	provider, responseID, err := realtimeUsageIdentity(body)
	if err != nil {
		return false, err
	}
	if o == nil {
		return false, errors.New("realtime usage outbox is not configured")
	}
	if !validRealtimeUsagePrincipal(principal) {
		return false, errors.New("realtime usage principal is required")
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.ensurePrincipalDir(principal); err != nil {
		return false, err
	}
	if err := o.recoverPendingLocked(principal); err != nil {
		return false, err
	}
	target := o.targetPath(principal, provider, responseID)
	legacyCanonical, legacyExists, legacyErr := o.migrateLegacyFile(principal, responseID)
	if legacyErr != nil {
		return false, fmt.Errorf("migrate legacy realtime usage outbox entry: %w", legacyErr)
	}
	if legacyExists && legacyCanonical == target {
		return false, nil
	}
	if _, err := os.Stat(target); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("check realtime usage outbox entry: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".pending-*")
	if err != nil {
		return false, fmt.Errorf("create realtime usage outbox temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return false, fmt.Errorf("protect realtime usage outbox temp: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		return false, fmt.Errorf("write realtime usage outbox temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return false, fmt.Errorf("sync realtime usage outbox temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close realtime usage outbox temp: %w", err)
	}
	if _, err := os.Stat(target); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("check realtime usage outbox entry: %w", err)
	}
	installed, installErr := installRealtimeUsageOutboxFile(tmpPath, target)
	if installErr != nil {
		return false, fmt.Errorf("install realtime usage outbox entry: %w", installErr)
	}
	if !installed {
		matched, verifyErr := o.canonicalEntryMatches(target, principal, provider, responseID)
		if verifyErr != nil {
			return false, fmt.Errorf("verify existing realtime usage outbox entry: %w", verifyErr)
		}
		if !matched {
			return false, errors.New("existing realtime usage outbox entry disappeared during install")
		}
		return false, nil
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		// The target is already visible, so leave it queued. A later replay can
		// still deliver it even if the directory fsync was unavailable.
		return true, fmt.Errorf("sync realtime usage outbox directory: %w", err)
	}
	return true, nil
}

// recoverPendingLocked completes a temp-file install left between fsync and
// rename by a process crash. It runs under o.mu and never overwrites an
// existing response-id entry. A partial or invalid temp is removed so it
// cannot block the queue forever on every restart.
func (o *realtimeUsageOutbox) recoverPendingLocked(principal string) error {
	if !validRealtimeUsagePrincipal(principal) {
		return errors.New("realtime usage principal is required")
	}
	entries, err := os.ReadDir(filepath.Join(o.dir, principal))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	changed := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".pending-") {
			continue
		}
		pendingPath := filepath.Join(o.dir, principal, entry.Name())
		body, readErr := os.ReadFile(pendingPath)
		provider, responseID, bodyErr := realtimeUsageIdentity(body)
		if readErr != nil || bodyErr != nil || len(body) == 0 || len(body) > realtimeUsageMaxBodyBytes {
			if removeErr := os.Remove(pendingPath); removeErr != nil && !os.IsNotExist(removeErr) && firstErr == nil {
				firstErr = fmt.Errorf("remove invalid realtime usage temp: %w", removeErr)
			}
			changed = true
			continue
		}
		if err := os.Chmod(pendingPath, 0o600); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("protect realtime usage temp: %w", err)
			}
			continue
		}
		pending, openErr := os.OpenFile(pendingPath, os.O_RDWR, 0o600)
		if openErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("open realtime usage temp: %w", openErr)
			}
			continue
		}
		syncErr := pending.Sync()
		closeErr := pending.Close()
		if syncErr != nil || closeErr != nil {
			if firstErr == nil {
				if syncErr != nil {
					firstErr = fmt.Errorf("sync realtime usage temp: %w", syncErr)
				} else {
					firstErr = fmt.Errorf("close realtime usage temp: %w", closeErr)
				}
			}
			continue
		}
		target := o.targetPath(principal, provider, responseID)
		if _, err := os.Stat(target); err == nil {
			if removeErr := os.Remove(pendingPath); removeErr != nil && !os.IsNotExist(removeErr) && firstErr == nil {
				firstErr = fmt.Errorf("remove duplicate realtime usage temp: %w", removeErr)
			}
			changed = true
			continue
		} else if !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = fmt.Errorf("check recovered realtime usage target: %w", err)
			}
			continue
		}
		installed, installErr := installRealtimeUsageOutboxFile(pendingPath, target)
		if installErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("recover realtime usage temp: %w", installErr)
			}
			continue
		}
		changed = true
		if !installed {
			if removeErr := os.Remove(pendingPath); removeErr != nil && !os.IsNotExist(removeErr) && firstErr == nil {
				firstErr = fmt.Errorf("remove duplicate realtime usage temp: %w", removeErr)
			}
		}
	}
	if changed {
		if err := syncDirectory(filepath.Join(o.dir, principal)); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("sync recovered realtime usage directory: %w", err)
		}
	}
	return firstErr
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (o *realtimeUsageOutbox) pendingPaths(principal string) ([]string, error) {
	if o == nil {
		return nil, errors.New("realtime usage outbox is not configured")
	}
	if !validRealtimeUsagePrincipal(principal) {
		return nil, errors.New("realtime usage principal is required")
	}
	entries, err := os.ReadDir(filepath.Join(o.dir, principal))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(o.dir, principal, entry.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

// replay retries every pending report. A failed report stays on disk and does
// not prevent later entries from being attempted. The first error is returned
// for observability; callers must not delete the failed file.
func (o *realtimeUsageOutbox) replay(ctx context.Context, principal string, send func(context.Context, json.RawMessage) error) error {
	if o == nil || send == nil {
		return nil
	}
	if !validRealtimeUsagePrincipal(principal) {
		return errors.New("realtime usage principal is required")
	}

	// Only protect filesystem snapshots and state transitions. Never hold the
	// outbox mutex across the provider request: usage POSTs must be able to
	// enqueue another durable report while a replay is waiting on Cloud.
	o.mu.Lock()
	recoveryErr := o.recoverPendingLocked(principal)
	if recoveryErr == nil {
		if migrationErr := o.migrateLegacyEntriesLocked(principal); migrationErr != nil {
			o.mu.Unlock()
			return migrationErr
		}
	}
	paths, err := o.pendingPaths(principal)
	o.mu.Unlock()
	if err != nil {
		return err
	}
	var firstErr error
	if recoveryErr != nil {
		firstErr = recoveryErr
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !o.claim(path) {
			continue
		}
		func() {
			defer o.release(path)
			body, err := os.ReadFile(path)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if _, err := realtimeUsageResponseID(body); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("invalid pending realtime usage: %w", err)
				}
				return
			}
			if err := send(ctx, json.RawMessage(body)); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			o.mu.Lock()
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				if firstErr == nil {
					firstErr = err
				}
			}
			o.mu.Unlock()
		}()
	}
	return firstErr
}

func (o *realtimeUsageOutbox) claim(path string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.inFlight == nil {
		o.inFlight = make(map[string]struct{})
	}
	if _, ok := o.inFlight[path]; ok {
		return false
	}
	o.inFlight[path] = struct{}{}
	return true
}

func (o *realtimeUsageOutbox) release(path string) {
	o.mu.Lock()
	delete(o.inFlight, path)
	o.mu.Unlock()
}

func (o *realtimeUsageOutbox) start(
	ctx context.Context,
	principal func() (string, bool),
	gateway func() *client.GatewayClient,
	send func(context.Context, *client.GatewayClient, string, json.RawMessage) error,
) {
	if o == nil || principal == nil || gateway == nil || send == nil {
		return
	}
	go func() {
		attempt := func() {
			currentPrincipal, ok := principal()
			gw := gateway()
			if !ok || !validRealtimeUsagePrincipal(currentPrincipal) || gw == nil {
				return
			}
			if err := o.replay(ctx, currentPrincipal, func(ctx context.Context, body json.RawMessage) error {
				return send(ctx, gw, currentPrincipal, body)
			}); err != nil && ctx.Err() == nil {
				// Keep provider response bodies out of logs; the raw payload remains
				// private on disk for the next attempt.
				log.Printf("daemon realtime usage outbox replay pending: %T", err)
			}
		}
		attempt()
		ticker := time.NewTicker(realtimeUsageOutboxInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				attempt()
			}
		}
	}()
}
