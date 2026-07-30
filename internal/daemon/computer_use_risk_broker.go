package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

const (
	// One pending entry represents one attended Desktop confirmation. Sixty
	// seconds covers ordinary read-and-click latency; a shorter bound causes
	// visible confirmation dialogs to expire while the user is reading them.
	// One grant covers only the immediate matching action, so ten seconds bounds
	// scheduler/GUI handoff delay without leaving reusable authority behind.
	// ConsequentialRiskBrokerOptions may lower either value for tests or embedded
	// callers, but the maxima are intentionally not operator-configurable:
	// lifting them would weaken the freshness guarantee rather than increase
	// supported workload.
	defaultConsequentialRiskPendingTTL = 60 * time.Second
	defaultConsequentialRiskGrantTTL   = 10 * time.Second
	maxConsequentialRiskPendingTTL     = 60 * time.Second
	maxConsequentialRiskGrantTTL       = 10 * time.Second
)

var (
	ErrConsequentialRiskIntentUnavailable = errors.New("consequential risk intent is unavailable")
	ErrConsequentialRiskGrantUnavailable  = errors.New("consequential risk grant is unavailable")
	ErrConsequentialRiskGrantMismatch     = errors.New("consequential risk grant binding mismatch")
	ErrConsequentialRiskDecisionInvalid   = errors.New("consequential risk decision is invalid")
	ErrConsequentialRiskDecisionCancelled = errors.New("consequential risk decision wait was cancelled")
	ErrConsequentialRiskIntentExpired     = errors.New("consequential risk intent expired")
)

type ConsequentialRiskBrokerOptions struct {
	Now        func() time.Time
	Random     io.Reader
	PendingTTL time.Duration
	GrantTTL   time.Duration
}

// ConsequentialRiskDraftV1 omits the two authority fields owned by the broker:
// the random intent ID and the short process-local expiry.
type ConsequentialRiskDraftV1 = tools.ConsequentialRiskDraftV1

type ConsequentialRiskDecision string

const (
	ConsequentialRiskDecisionAllow   ConsequentialRiskDecision = "allow"
	ConsequentialRiskDecisionDeny    ConsequentialRiskDecision = "deny"
	ConsequentialRiskDecisionAllowed ConsequentialRiskDecision = "allowed"
	ConsequentialRiskDecisionDenied  ConsequentialRiskDecision = "denied"
)

type ConsequentialRiskDecisionRequestV1 struct {
	SchemaVersion int                       `json:"schema_version"`
	IntentID      string                    `json:"intent_id"`
	Decision      ConsequentialRiskDecision `json:"decision"`
}

type ConsequentialRiskDecisionResponseV1 struct {
	SchemaVersion  int                       `json:"schema_version"`
	IntentID       string                    `json:"intent_id"`
	Decision       ConsequentialRiskDecision `json:"decision"`
	GrantExpiresAt *string                   `json:"grant_expires_at"`
}

// ConsequentialRiskGrantClaimV1 is the exact execution-side claim consumed at
// the final commit boundary. It is never an HTTP or event payload.
type ConsequentialRiskGrantClaimV1 struct {
	IntentID     string
	RequestID    string
	TargetDigest string
	Kind         string
	Send         *tools.ConsequentialSendDetailV1
	Delete       *tools.ConsequentialDeleteDetailV1
	Purchase     *tools.ConsequentialPurchaseDetailV1
}

type consequentialRiskGrant struct {
	intentID     string
	requestID    string
	targetDigest string
	expiresAt    time.Time
	approved     tools.ConsequentialRiskDraftV1
}

type consequentialRiskNotice struct {
	requestID string
	expiresAt time.Time
	result    chan consequentialRiskWaitResult
}

type consequentialRiskWaitResult struct {
	response ConsequentialRiskDecisionResponseV1
	err      error
}

// ConsequentialRiskBroker owns only process memory. Authoritative labels and
// target digests never enter daemon events, logs, or persistent state here.
type ConsequentialRiskBroker struct {
	mu sync.Mutex

	now        func() time.Time
	random     io.Reader
	pendingTTL time.Duration
	grantTTL   time.Duration
	pending    map[string]tools.ConsequentialRiskIntentV1
	grants     map[string]consequentialRiskGrant
	notices    map[string]consequentialRiskNotice
}

func NewConsequentialRiskBroker(options ConsequentialRiskBrokerOptions) (*ConsequentialRiskBroker, error) {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.PendingTTL == 0 {
		options.PendingTTL = defaultConsequentialRiskPendingTTL
	}
	if options.GrantTTL == 0 {
		options.GrantTTL = defaultConsequentialRiskGrantTTL
	}
	if options.PendingTTL < time.Second || options.PendingTTL > maxConsequentialRiskPendingTTL {
		return nil, fmt.Errorf("consequential risk pending TTL must be between one and 60 seconds")
	}
	if options.GrantTTL < time.Second || options.GrantTTL > maxConsequentialRiskGrantTTL {
		return nil, fmt.Errorf("consequential risk grant TTL must be between one and 10 seconds")
	}
	return &ConsequentialRiskBroker{
		now:        options.Now,
		random:     options.Random,
		pendingTTL: options.PendingTTL,
		grantTTL:   options.GrantTTL,
		pending:    make(map[string]tools.ConsequentialRiskIntentV1),
		grants:     make(map[string]consequentialRiskGrant),
		notices:    make(map[string]consequentialRiskNotice),
	}, nil
}

func newDefaultConsequentialRiskBroker() *ConsequentialRiskBroker {
	broker, err := NewConsequentialRiskBroker(ConsequentialRiskBrokerOptions{})
	if err != nil {
		panic("invalid default consequential risk broker options: " + err.Error())
	}
	return broker
}

// Register mints an exact pending intent and its only persistence-safe
// projection. The returned authoritative intent is for the local detail seam;
// callers must never log or persist it.
func (b *ConsequentialRiskBroker) Register(draft ConsequentialRiskDraftV1) (
	tools.ConsequentialRiskIntentV1,
	tools.ConsequentialRiskMarkerV1,
	error,
) {
	if b == nil {
		return tools.ConsequentialRiskIntentV1{}, tools.ConsequentialRiskMarkerV1{},
			fmt.Errorf("consequential risk broker is unavailable")
	}
	now := b.now().UTC()
	expiresAt := now.Add(b.pendingTTL).Truncate(time.Second)
	if coordinate := draft.Target.CoordinateAuthority; coordinate != nil {
		frameExpiry, err := time.Parse(time.RFC3339Nano, coordinate.FrameExpiresAt)
		if err != nil {
			return tools.ConsequentialRiskIntentV1{}, tools.ConsequentialRiskMarkerV1{},
				fmt.Errorf("invalid consequential risk coordinate frame expiry: %w", err)
		}
		frameExpiry = frameExpiry.UTC().Truncate(time.Second)
		if frameExpiry.Before(expiresAt) {
			expiresAt = frameExpiry
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeExpiredLocked(now)
	randomID := make([]byte, 16)
	if _, err := io.ReadFull(b.random, randomID); err != nil {
		return tools.ConsequentialRiskIntentV1{}, tools.ConsequentialRiskMarkerV1{},
			fmt.Errorf("mint consequential risk intent ID: %w", err)
	}
	intentID := "cri_" + base64.RawURLEncoding.EncodeToString(randomID)
	if _, found := b.pending[intentID]; found {
		return tools.ConsequentialRiskIntentV1{}, tools.ConsequentialRiskMarkerV1{},
			fmt.Errorf("mint consequential risk intent ID: random collision")
	}
	if _, found := b.grants[intentID]; found {
		return tools.ConsequentialRiskIntentV1{}, tools.ConsequentialRiskMarkerV1{},
			fmt.Errorf("mint consequential risk intent ID: random collision")
	}
	intent := tools.ConsequentialRiskIntentV1{
		SchemaVersion: 1,
		IntentID:      intentID,
		RequestID:     draft.RequestID,
		Kind:          draft.Kind,
		ExpiresAt:     expiresAt.Format(time.RFC3339),
		Target:        draft.Target,
		Send:          cloneConsequentialSendDetail(draft.Send),
		Delete:        cloneConsequentialDeleteDetail(draft.Delete),
		Purchase:      cloneConsequentialPurchaseDetail(draft.Purchase),
	}
	if err := intent.Validate(now); err != nil {
		return tools.ConsequentialRiskIntentV1{}, tools.ConsequentialRiskMarkerV1{}, err
	}
	marker, err := intent.PersistentMarker(now)
	if err != nil {
		return tools.ConsequentialRiskIntentV1{}, tools.ConsequentialRiskMarkerV1{}, err
	}
	b.pending[intentID] = cloneConsequentialRiskIntent(intent)
	b.notices[intentID] = consequentialRiskNotice{
		requestID: draft.RequestID, expiresAt: expiresAt,
		result: make(chan consequentialRiskWaitResult, 1),
	}
	return cloneConsequentialRiskIntent(intent), marker, nil
}

// AwaitDecision is notification-driven and safe when the HTTP decision wins
// the race before this method starts. Cancellation atomically invalidates any
// pending intent or freshly minted grant.
func (b *ConsequentialRiskBroker) AwaitDecision(ctx context.Context, intentID string) (ConsequentialRiskDecisionResponseV1, error) {
	if b == nil {
		return ConsequentialRiskDecisionResponseV1{}, ErrConsequentialRiskIntentUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	b.mu.Lock()
	notice, found := b.notices[intentID]
	if !found {
		b.mu.Unlock()
		return ConsequentialRiskDecisionResponseV1{}, ErrConsequentialRiskIntentUnavailable
	}
	delay := notice.expiresAt.Sub(b.now().UTC())
	if delay < 0 {
		delay = 0
	}
	b.mu.Unlock()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case result := <-notice.result:
		b.mu.Lock()
		delete(b.notices, intentID)
		b.mu.Unlock()
		return result.response, result.err
	case <-ctx.Done():
		b.invalidateIntentWithError(intentID, ErrConsequentialRiskDecisionCancelled)
		return ConsequentialRiskDecisionResponseV1{}, ErrConsequentialRiskDecisionCancelled
	case <-timer.C:
		b.invalidateIntentWithError(intentID, ErrConsequentialRiskIntentExpired)
		return ConsequentialRiskDecisionResponseV1{}, ErrConsequentialRiskIntentExpired
	}
}

func (b *ConsequentialRiskBroker) Detail(intentID string) (tools.ConsequentialRiskIntentV1, error) {
	if b == nil {
		return tools.ConsequentialRiskIntentV1{}, ErrConsequentialRiskIntentUnavailable
	}
	now := b.now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeExpiredLocked(now)
	intent, found := b.pending[intentID]
	if !found {
		return tools.ConsequentialRiskIntentV1{}, ErrConsequentialRiskIntentUnavailable
	}
	return cloneConsequentialRiskIntent(intent), nil
}

func (b *ConsequentialRiskBroker) Decide(request ConsequentialRiskDecisionRequestV1) (
	ConsequentialRiskDecisionResponseV1,
	error,
) {
	if b == nil {
		return ConsequentialRiskDecisionResponseV1{}, ErrConsequentialRiskIntentUnavailable
	}
	if request.SchemaVersion != 1 || request.IntentID == "" ||
		(request.Decision != ConsequentialRiskDecisionAllow && request.Decision != ConsequentialRiskDecisionDeny) {
		return ConsequentialRiskDecisionResponseV1{}, ErrConsequentialRiskDecisionInvalid
	}
	now := b.now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeExpiredLocked(now)
	intent, found := b.pending[request.IntentID]
	if !found {
		return ConsequentialRiskDecisionResponseV1{}, ErrConsequentialRiskIntentUnavailable
	}
	delete(b.pending, request.IntentID)
	delete(b.grants, request.IntentID)
	response := ConsequentialRiskDecisionResponseV1{
		SchemaVersion: 1,
		IntentID:      request.IntentID,
	}
	if request.Decision == ConsequentialRiskDecisionDeny {
		response.Decision = ConsequentialRiskDecisionDenied
		b.notifyLocked(request.IntentID, consequentialRiskWaitResult{response: response})
		return response, nil
	}
	intentExpiry, err := time.Parse(time.RFC3339, intent.ExpiresAt)
	if err != nil {
		return ConsequentialRiskDecisionResponseV1{}, ErrConsequentialRiskIntentUnavailable
	}
	grantExpiry := now.Add(b.grantTTL)
	if intentExpiry.Before(grantExpiry) {
		grantExpiry = intentExpiry
	}
	if !grantExpiry.After(now) {
		return ConsequentialRiskDecisionResponseV1{}, ErrConsequentialRiskIntentUnavailable
	}
	b.grants[request.IntentID] = consequentialRiskGrant{
		intentID:     request.IntentID,
		requestID:    intent.RequestID,
		targetDigest: intent.Target.TargetDigest,
		expiresAt:    grantExpiry,
		approved: tools.ConsequentialRiskDraftV1{
			RequestID: intent.RequestID, Kind: intent.Kind, Target: intent.Target,
			Send: cloneConsequentialSendDetail(intent.Send), Delete: cloneConsequentialDeleteDetail(intent.Delete),
			Purchase: cloneConsequentialPurchaseDetail(intent.Purchase),
		},
	}
	formattedExpiry := grantExpiry.UTC().Truncate(time.Second).Format(time.RFC3339)
	response.Decision = ConsequentialRiskDecisionAllowed
	response.GrantExpiresAt = &formattedExpiry
	b.notifyLocked(request.IntentID, consequentialRiskWaitResult{response: response})
	return response, nil
}

// ConsumeGrant burns the located grant before checking its exact binding. A
// mismatched attempt cannot be corrected or replayed against the same grant.
func (b *ConsequentialRiskBroker) ConsumeGrant(claim ConsequentialRiskGrantClaimV1) error {
	if b == nil {
		return ErrConsequentialRiskGrantUnavailable
	}
	now := b.now().UTC()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeExpiredLocked(now)
	grant, found := b.grants[claim.IntentID]
	if !found {
		return ErrConsequentialRiskGrantUnavailable
	}
	delete(b.grants, claim.IntentID)
	claimed := tools.ConsequentialRiskDraftV1{
		RequestID: claim.RequestID, Kind: claim.Kind,
		Target: tools.ConsequentialRiskTargetAuthorityV1{TargetDigest: claim.TargetDigest},
		Send:   claim.Send, Delete: claim.Delete, Purchase: claim.Purchase,
	}
	// ExecutionScope keeps only the content-free identity/digest. At the burn
	// seam compare the full approved detail separately so a destination swap
	// cannot reuse an otherwise identical AX target grant.
	if grant.intentID != claim.IntentID || grant.requestID != claim.RequestID ||
		grant.targetDigest != claim.TargetDigest || grant.approved.Kind != claimed.Kind ||
		!equalConsequentialClaimDetails(grant.approved, claimed) {
		return ErrConsequentialRiskGrantMismatch
	}
	return nil
}

// InvalidateIntent is the cancellation/Stop/lease-loss hook for one exact
// confirmation. Its count is content-free and safe for internal assertions.
func (b *ConsequentialRiskBroker) InvalidateIntent(intentID string) int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	removed := 0
	if _, found := b.pending[intentID]; found {
		delete(b.pending, intentID)
		removed++
	}
	if _, found := b.grants[intentID]; found {
		delete(b.grants, intentID)
		removed++
	}
	b.notifyLocked(intentID, consequentialRiskWaitResult{err: ErrConsequentialRiskDecisionCancelled})
	return removed
}

// InvalidateRequest removes every pending intent and grant owned by a stopped
// request without exposing any of their target details.
func (b *ConsequentialRiskBroker) InvalidateRequest(requestID string) int {
	if b == nil || requestID == "" {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	removed := 0
	for intentID, intent := range b.pending {
		if intent.RequestID == requestID {
			delete(b.pending, intentID)
			removed++
			b.notifyLocked(intentID, consequentialRiskWaitResult{err: ErrConsequentialRiskDecisionCancelled})
		}
	}
	for intentID, grant := range b.grants {
		if grant.requestID == requestID {
			delete(b.grants, intentID)
			removed++
			b.notifyLocked(intentID, consequentialRiskWaitResult{err: ErrConsequentialRiskDecisionCancelled})
		}
	}
	return removed
}

func (b *ConsequentialRiskBroker) InvalidateAll() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	removed := len(b.pending) + len(b.grants)
	for intentID := range b.notices {
		b.notifyLocked(intentID, consequentialRiskWaitResult{err: ErrConsequentialRiskDecisionCancelled})
	}
	clear(b.pending)
	clear(b.grants)
	return removed
}

func (b *ConsequentialRiskBroker) purgeExpiredLocked(now time.Time) {
	for intentID, intent := range b.pending {
		expiresAt, err := time.Parse(time.RFC3339, intent.ExpiresAt)
		if err != nil || !now.Before(expiresAt) {
			delete(b.pending, intentID)
			b.notifyLocked(intentID, consequentialRiskWaitResult{err: ErrConsequentialRiskIntentExpired})
		}
	}
	for intentID, grant := range b.grants {
		if !now.Before(grant.expiresAt) {
			delete(b.grants, intentID)
		}
	}
}

func (b *ConsequentialRiskBroker) notifyLocked(intentID string, result consequentialRiskWaitResult) {
	if notice, found := b.notices[intentID]; found {
		select {
		case notice.result <- result:
		default:
		}
	}
}

func (b *ConsequentialRiskBroker) invalidateIntentWithError(intentID string, reason error) {
	b.mu.Lock()
	delete(b.pending, intentID)
	delete(b.grants, intentID)
	b.notifyLocked(intentID, consequentialRiskWaitResult{err: reason})
	delete(b.notices, intentID)
	b.mu.Unlock()
}

func equalConsequentialClaimDetails(a, b tools.ConsequentialRiskDraftV1) bool {
	a.Target = tools.ConsequentialRiskTargetAuthorityV1{}
	b.Target = tools.ConsequentialRiskTargetAuthorityV1{}
	a.RequestID = ""
	b.RequestID = ""
	return tools.EqualConsequentialRiskDraftV1(a, b)
}

func cloneConsequentialRiskIntent(intent tools.ConsequentialRiskIntentV1) tools.ConsequentialRiskIntentV1 {
	if intent.Target.CoordinateAuthority != nil {
		coordinate := *intent.Target.CoordinateAuthority
		intent.Target.CoordinateAuthority = &coordinate
	}
	intent.Send = cloneConsequentialSendDetail(intent.Send)
	intent.Delete = cloneConsequentialDeleteDetail(intent.Delete)
	intent.Purchase = cloneConsequentialPurchaseDetail(intent.Purchase)
	return intent
}

func cloneConsequentialSendDetail(detail *tools.ConsequentialSendDetailV1) *tools.ConsequentialSendDetailV1 {
	if detail == nil {
		return nil
	}
	clone := *detail
	return &clone
}

func cloneConsequentialDeleteDetail(detail *tools.ConsequentialDeleteDetailV1) *tools.ConsequentialDeleteDetailV1 {
	if detail == nil {
		return nil
	}
	clone := *detail
	return &clone
}

func cloneConsequentialPurchaseDetail(detail *tools.ConsequentialPurchaseDetailV1) *tools.ConsequentialPurchaseDetailV1 {
	if detail == nil {
		return nil
	}
	clone := *detail
	return &clone
}
