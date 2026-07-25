package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestToolRegistry_Get(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "file_read"})

	tool, ok := reg.Get("file_read")
	if !ok {
		t.Fatal("expected to find file_read")
	}
	if tool.Info().Name != "file_read" {
		t.Errorf("expected 'file_read', got %q", tool.Info().Name)
	}

	_, ok = reg.Get("nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestToolRegistry_Schemas(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "file_read"})
	reg.Register(&mockTool{name: "bash"})

	schemas := reg.Schemas()
	if len(schemas) != 2 {
		t.Errorf("expected 2 schemas, got %d", len(schemas))
	}
}

type mockTool struct {
	name     string
	required []string     // optional: names of required fields advertised via Info()
	runs     atomic.Int32 // tests can read to assert Run() was/was not invoked
}

func (m *mockTool) Info() ToolInfo {
	return ToolInfo{
		Name:        m.name,
		Description: "mock tool",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Required:    m.required,
	}
}

func (m *mockTool) Run(ctx context.Context, args string) (ToolResult, error) {
	m.runs.Add(1)
	return ToolResult{Content: "mock result"}, nil
}

func (m *mockTool) RequiresApproval() bool { return false }

// TestDisallowsAutoApproval pins the attended policy. computer_use may be
// explicitly persisted as the product's global Computer Use grant; legacy GUI
// tools remain excluded so they cannot bypass the new guarded surface.
func TestDisallowsAutoApproval(t *testing.T) {
	want := []string{"computer", "accessibility", "applescript", "ghostty"}
	got := AutoApprovalDenyList()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("autoApprovalDenyList expected %v, got %v", want, got)
	}
	for _, name := range want {
		if !DisallowsAutoApproval(name) {
			t.Fatalf("GUI control tool %s must require fresh approval", name)
		}
	}
	// Former entries remain persistable; this change is scoped to live GUI control.
	for _, name := range []string{
		"publish_to_web", "generate_image", "edit_image",
		"computer_use", "bash", "file_write", "cloud_delegate", "think",
	} {
		if DisallowsAutoApproval(name) {
			t.Fatalf("%s should not be in the per-call approval denylist", name)
		}
	}
}

func TestGUIActionOutcomeValidationRequiresCoherentFailureCodes(t *testing.T) {
	validPointer := &GUIActionPointer{
		DisplayID: 1, TopologyID: "topology-1", TopologyGeneration: 1, X: -10, Y: 20,
	}
	for _, tc := range []struct {
		name    string
		outcome GUIActionOutcome
		valid   bool
	}{
		{name: "verified", outcome: GUIActionOutcome{Result: GUIActionResultVerified, Phase: GUIActionPhaseVerifying, Pointer: validPointer}, valid: true},
		{name: "verified with failure", outcome: GUIActionOutcome{Result: GUIActionResultVerified, Phase: GUIActionPhaseVerifying, FailureCode: "unexpected"}},
		{name: "unverified with reason", outcome: GUIActionOutcome{Result: GUIActionResultCompletedUnverified, Phase: GUIActionPhaseVerifying, FailureCode: "postcondition_not_observed"}, valid: true},
		{name: "unverified without reason", outcome: GUIActionOutcome{Result: GUIActionResultCompletedUnverified, Phase: GUIActionPhaseVerifying}},
		{name: "failed with unsafe code", outcome: GUIActionOutcome{Result: GUIActionResultFailed, Phase: GUIActionPhaseActing, FailureCode: "secret: leaked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.outcome.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate() error=%v valid=%v", err, tc.valid)
			}
		})
	}
}

// TestDisallowsUnattendedAutoApproval pins the unattended gate. The list
// contains computer_use, standalone screenshot, and every legacy GUI-control
// name — an unattended schedule/heartbeat/watcher run must never observe or
// control the screen on the strength of an attended always-allow click or a
// blanket auto_approve, nor use a legacy name to bypass the computer_use gate.
// See unattendedAutoApprovalDenyList for the full rationale.
//
// If you add an entry, also enumerate the tools you expect to remain off
// the list so accidental over-broad deny-listing (which would break
// scheduled runs of ordinary agents) gets caught.
func TestDisallowsUnattendedAutoApproval(t *testing.T) {
	got := UnattendedAutoApprovalDenyList()
	want := []string{"computer_use", "screenshot", "computer", "accessibility", "applescript", "ghostty"}
	if len(got) != len(want) {
		t.Fatalf("unattendedAutoApprovalDenyList expected %v, got %v", want, got)
	}
	for index, name := range want {
		if got[index] != name || !DisallowsUnattendedAutoApproval(name) {
			t.Fatalf("GUI tool %q missing from unattended deny-list %v", name, got)
		}
	}
	if !DisallowsUnattendedAutoApproval("screenshot") {
		t.Fatal("screenshot must be refused unattended auto-approval")
	}
	// Ordinary tools and the three formerly-deny-listed tools should ALL
	// return false. The formerly-deny-listed trio is enumerated explicitly
	// so a regression that re-adds them to the unattended list — without
	// also moving them off this assertion — fails loudly in review.
	for _, name := range []string{
		"publish_to_web", "generate_image", "edit_image",
		"bash", "file_write", "file_read", "think", "cloud_delegate", "browser",
	} {
		if DisallowsUnattendedAutoApproval(name) {
			t.Errorf("%s should NOT be on the unattended deny-list", name)
		}
	}
}

// TestHighRiskListConsistency guards against drift between
// agent.autoApprovalDenyList (runtime approval gate) and the agents package's
// highRiskTools (persistence gate). The two lists encode the same security
// policy from different sides; if they drift, a user could persist a tool
// into always-allow that the runtime still prompts on, or vice versa.
//
// This test does set equality, not a hardcoded probe — adding a new entry to
// one list without the mirror will fail this test immediately.
func TestHighRiskListConsistency(t *testing.T) {
	runtime := AutoApprovalDenyList()
	persistence := agents.HighRiskTools()

	if len(runtime) != len(persistence) {
		t.Fatalf("size mismatch: agent.AutoApprovalDenyList=%d agents.HighRiskTools=%d (runtime=%v persistence=%v)",
			len(runtime), len(persistence), runtime, persistence)
	}

	rtSet := make(map[string]struct{}, len(runtime))
	for _, t := range runtime {
		rtSet[t] = struct{}{}
	}
	for _, name := range persistence {
		if _, ok := rtSet[name]; !ok {
			t.Errorf("agents.HighRiskTools contains %q but agent.AutoApprovalDenyList does not", name)
		}
		if agents.IsToolAlwaysAllowable(name) {
			t.Errorf("agents.IsToolAlwaysAllowable(%q) = true; want false", name)
		}
		if !DisallowsAutoApproval(name) {
			t.Errorf("DisallowsAutoApproval(%q) = false; want true", name)
		}
	}

	// Spot-check ordinary safe tools — both gates must agree they're allowed.
	for _, name := range []string{"file_write", "http", "bash", "file_read", "browser_navigate", "think"} {
		if _, denied := rtSet[name]; denied {
			t.Errorf("autoApprovalDenyList accidentally contains safe tool %q", name)
		}
		if DisallowsAutoApproval(name) {
			t.Errorf("DisallowsAutoApproval(%q) = true; want false", name)
		}
		if !agents.IsToolAlwaysAllowable(name) {
			t.Errorf("agents.IsToolAlwaysAllowable(%q) = false; want true", name)
		}
	}

	// Computer Use is persistable only as the single global product
	// permission. Runtime auto-approval may honor that global grant, while the
	// per-agent persistence gate must reject a second scoped grant.
	if DisallowsAutoApproval("computer_use") {
		t.Error("global computer_use grant was disabled")
	}
	if agents.IsToolAlwaysAllowable("computer_use") {
		t.Error("computer_use was admitted to per-agent always-allow")
	}
}

type mockNativeTool struct {
	name string
	def  *client.NativeToolDef
}

func (m *mockNativeTool) Info() ToolInfo {
	return ToolInfo{Name: m.name, Description: "native tool"}
}
func (m *mockNativeTool) Run(ctx context.Context, args string) (ToolResult, error) {
	return ToolResult{Content: "ok"}, nil
}
func (m *mockNativeTool) RequiresApproval() bool { return false }
func (m *mockNativeTool) NativeToolDef() *client.NativeToolDef {
	if m.def != nil {
		return m.def
	}
	return &client.NativeToolDef{
		Type:            "computer_20251124",
		Name:            "computer",
		DisplayWidthPx:  1280,
		DisplayHeightPx: 800,
	}
}

type preparingMockNativeTool struct {
	*mockNativeTool
	prepareCalls int
}

func (m *preparingMockNativeTool) PrepareNativeToolRequest(context.Context) error {
	m.prepareCalls++
	return nil
}

func TestPrepareProviderNativeToolsHonorsSelectedTaggedUnion(t *testing.T) {
	native := &preparingMockNativeTool{mockNativeTool: &mockNativeTool{name: "computer"}}
	reg := NewToolRegistry()
	reg.Register(native)

	selectedFunction := client.Tool{
		Type: "function",
		Function: client.FunctionDef{
			Name:       "computer",
			Parameters: map[string]any{"type": "object"},
		},
		DeferLoading: true,
	}
	if err := prepareProviderNativeTools(context.Background(), reg, []client.Tool{selectedFunction}); err != nil {
		t.Fatalf("function selection preparation: %v", err)
	}
	if native.prepareCalls != 0 {
		t.Fatalf("same-name function selection triggered %d native preparations", native.prepareCalls)
	}

	selectedNative := buildToolSchema(native)
	if err := prepareProviderNativeTools(
		context.Background(), reg, []client.Tool{selectedNative, selectedNative},
	); err != nil {
		t.Fatalf("native selection preparation: %v", err)
	}
	if native.prepareCalls != 1 {
		t.Fatalf("duplicate selected native schemas triggered %d preparations, want 1", native.prepareCalls)
	}
}

func TestRefreshProviderNativeToolSchemasOnlyRebuildsNativeEntries(t *testing.T) {
	nativeDef := &client.NativeToolDef{
		Type:            client.NativeComputerToolType,
		Name:            client.NativeComputerToolName,
		DisplayWidthPx:  1280,
		DisplayHeightPx: 800,
	}
	reg := NewToolRegistry()
	reg.Register(&mockNativeTool{name: "computer", def: nativeDef})
	reg.Register(&mockTool{name: "file_read"})

	functionSchema := buildToolSchema(&mockTool{name: "file_read"})
	functionSchema.DeferLoading = true
	before := []client.Tool{
		buildToolSchema(&mockNativeTool{name: "computer", def: nativeDef}),
		functionSchema,
	}

	nativeDef.DisplayWidthPx = 1024
	nativeDef.DisplayHeightPx = 768
	got := refreshProviderNativeToolSchemas(reg, before)

	if got[0].DisplayWidthPx != 1024 || got[0].DisplayHeightPx != 768 {
		t.Fatalf("native dimensions = %dx%d, want 1024x768", got[0].DisplayWidthPx, got[0].DisplayHeightPx)
	}
	if !reflect.DeepEqual(got[1], before[1]) {
		t.Fatalf("ordinary function schema changed:\n got: %+v\nwant: %+v", got[1], before[1])
	}
	if got[1].DeferLoading != true {
		t.Fatal("ordinary function lost defer_loading while refreshing native schema")
	}
	if &got[0] == &before[0] {
		t.Fatal("refresh must return an isolated slice so prior dispatched requests stay immutable")
	}

	selectedFunction := client.Tool{
		Type: "function",
		Function: client.FunctionDef{
			Name:       "computer",
			Parameters: map[string]any{"type": "object"},
		},
		DeferLoading: true,
	}
	functionSelected := refreshProviderNativeToolSchemas(reg, []client.Tool{selectedFunction})
	if !reflect.DeepEqual(functionSelected[0], selectedFunction) {
		t.Fatalf("native provider was mixed into a function/deferred selection:\n got: %+v\nwant: %+v", functionSelected[0], selectedFunction)
	}
}

func TestBuildToolSchemaRejectsInvalidNativeDefinitionAtWireBoundary(t *testing.T) {
	tests := []struct {
		name string
		def  client.NativeToolDef
	}{
		{name: "wrong type", def: client.NativeToolDef{Type: "computer_20241022", Name: "computer", DisplayWidthPx: 1280, DisplayHeightPx: 800}},
		{name: "wrong name", def: client.NativeToolDef{Type: "computer_20251124", Name: "desktop", DisplayWidthPx: 1280, DisplayHeightPx: 800}},
		{name: "zero width", def: client.NativeToolDef{Type: "computer_20251124", Name: "computer", DisplayHeightPx: 800}},
		{name: "negative height", def: client.NativeToolDef{Type: "computer_20251124", Name: "computer", DisplayWidthPx: 1280, DisplayHeightPx: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			schema := buildToolSchema(&mockNativeTool{name: "computer", def: &tc.def})
			if err := schema.Validate(); err == nil {
				t.Fatalf("invalid native definition passed schema validation: %+v", schema)
			}
			if _, err := json.Marshal(schema); err == nil {
				t.Fatalf("invalid native definition marshaled successfully: %+v", schema)
			}
		})
	}
}

func TestToolRegistry_SchemasIncludesNativeTool(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockNativeTool{name: "computer"})
	reg.Register(&mockTool{name: "bash"})

	schemas := reg.Schemas()
	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(schemas))
	}
	// Native tool should use its own type
	if schemas[0].Type != "computer_20251124" {
		t.Errorf("expected type 'computer_20251124', got %q", schemas[0].Type)
	}
	if schemas[0].Name != "computer" {
		t.Errorf("expected name 'computer', got %q", schemas[0].Name)
	}
	if schemas[0].DisplayWidthPx != 1280 {
		t.Errorf("expected display_width_px 1280, got %d", schemas[0].DisplayWidthPx)
	}
	// Standard tool should use function type
	if schemas[1].Type != "function" {
		t.Errorf("expected type 'function' for bash, got %q", schemas[1].Type)
	}
}

func TestToolRegistry_Remove(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "a"})
	r.Register(&mockTool{name: "b"})
	r.Register(&mockTool{name: "c"})

	r.Remove("b")

	if _, ok := r.Get("b"); ok {
		t.Error("b should be removed")
	}
	if r.Len() != 2 {
		t.Errorf("Len() = %d, want 2", r.Len())
	}
	names := r.Names()
	if len(names) != 2 || names[0] != "a" || names[1] != "c" {
		t.Errorf("names = %v, want [a c]", names)
	}
}

func TestToolRegistry_RemoveNonexistent(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "a"})
	r.Remove("nonexistent") // should not panic
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want 1", r.Len())
	}
}

func TestToolRegistry_FilterByAllow(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "file_read"})
	r.Register(&mockTool{name: "bash"})
	r.Register(&mockTool{name: "computer"})
	r.Register(&mockTool{name: "browser"})

	filtered := r.FilterByAllow([]string{"file_read", "bash"})
	if filtered.Len() != 2 {
		t.Errorf("filtered Len() = %d, want 2", filtered.Len())
	}
	if _, ok := filtered.Get("computer"); ok {
		t.Error("computer should be filtered out")
	}
	if _, ok := filtered.Get("file_read"); !ok {
		t.Error("file_read should be present")
	}
}

func TestToolRegistry_FilterByDeny(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "file_read"})
	r.Register(&mockTool{name: "bash"})
	r.Register(&mockTool{name: "computer"})
	r.Register(&mockTool{name: "browser"})

	filtered := r.FilterByDeny([]string{"computer", "browser"})
	if filtered.Len() != 2 {
		t.Errorf("filtered Len() = %d, want 2", filtered.Len())
	}
	if _, ok := filtered.Get("computer"); ok {
		t.Error("computer should be denied")
	}
	if _, ok := filtered.Get("file_read"); !ok {
		t.Error("file_read should be present")
	}
}

func TestToolRegistry_CloneIndependence(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "a"})
	r.Register(&mockTool{name: "b"})

	c := r.Clone()
	c.Remove("a")

	if _, ok := r.Get("a"); !ok {
		t.Error("original should still have 'a'")
	}
	if c.Len() != 1 {
		t.Errorf("clone Len() = %d, want 1", c.Len())
	}
}

func TestToolRegistry_RegisterOverwrite(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "a"})
	r.Register(&mockTool{name: "b"})
	r.Register(&mockTool{name: "a"}) // overwrite

	names := r.Names()
	if len(names) != 2 {
		t.Errorf("expected 2 names after overwrite, got %d: %v", len(names), names)
	}
	if r.Len() != 2 {
		t.Errorf("Len() = %d, want 2", r.Len())
	}
	schemas := r.Schemas()
	if len(schemas) != 2 {
		t.Errorf("expected 2 schemas, got %d", len(schemas))
	}
}

func TestToolRegistry_RemoveAndReRegister(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "a"})
	r.Register(&mockTool{name: "b"})
	r.Remove("a")
	r.Register(&mockTool{name: "a"})

	names := r.Names()
	if len(names) != 2 {
		t.Errorf("expected 2 names, got %d: %v", len(names), names)
	}
	schemas := r.Schemas()
	if len(schemas) != 2 {
		t.Errorf("expected 2 schemas, got %d", len(schemas))
	}
}

func TestToolResultErrorHelpers(t *testing.T) {
	tests := []struct {
		name        string
		result      ToolResult
		wantIsError bool
		wantCat     ErrorCategory
		wantRetry   bool
		wantPrefix  string
	}{
		{
			name:        "TransientError",
			result:      TransientError("connection timed out"),
			wantIsError: true,
			wantCat:     ErrCategoryTransient,
			wantRetry:   true,
			wantPrefix:  "[transient error]",
		},
		{
			name:        "ValidationError",
			result:      ValidationError("invalid URL format"),
			wantIsError: true,
			wantCat:     ErrCategoryValidation,
			wantRetry:   false,
			wantPrefix:  "[validation error]",
		},
		{
			name:        "BusinessError",
			result:      BusinessError("refund exceeds policy limit"),
			wantIsError: true,
			wantCat:     ErrCategoryBusiness,
			wantRetry:   false,
			wantPrefix:  "[business error]",
		},
		{
			name:        "PermissionError",
			result:      PermissionError("access denied"),
			wantIsError: true,
			wantCat:     ErrCategoryPermission,
			wantRetry:   false,
			wantPrefix:  "[permission error]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.result.IsError != tt.wantIsError {
				t.Errorf("IsError = %v, want %v", tt.result.IsError, tt.wantIsError)
			}
			if tt.result.ErrorCategory != tt.wantCat {
				t.Errorf("ErrorCategory = %q, want %q", tt.result.ErrorCategory, tt.wantCat)
			}
			if tt.result.IsRetryable != tt.wantRetry {
				t.Errorf("IsRetryable = %v, want %v", tt.result.IsRetryable, tt.wantRetry)
			}
			if !strings.HasPrefix(tt.result.Content, tt.wantPrefix) {
				t.Errorf("Content = %q, want prefix %q", tt.result.Content, tt.wantPrefix)
			}
		})
	}
}

func TestToolResult_ZeroValueNotError(t *testing.T) {
	r := ToolResult{Content: "some output"}
	if r.IsError {
		t.Error("zero-value ToolResult must not be an error")
	}
	if r.ErrorCategory != "" {
		t.Errorf("zero-value ErrorCategory must be empty, got %q", r.ErrorCategory)
	}
	if r.IsRetryable {
		t.Error("zero-value IsRetryable must be false")
	}
}

func TestToolResult_ImagesField(t *testing.T) {
	result := ToolResult{
		Content: "Screenshot captured",
		IsError: false,
		Images: []ImageBlock{
			{MediaType: "image/png", Data: "iVBORfakedata"},
		},
	}
	if len(result.Images) != 1 {
		t.Errorf("expected 1 image, got %d", len(result.Images))
	}
	if result.Images[0].MediaType != "image/png" {
		t.Errorf("expected image/png, got %s", result.Images[0].MediaType)
	}
}

// mockSourcedTool is a mock tool that implements ToolSourcer.
type mockSourcedTool struct {
	name   string
	source ToolSource
}

func (m *mockSourcedTool) Info() ToolInfo {
	return ToolInfo{
		Name:        m.name,
		Description: "mock sourced tool",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}
func (m *mockSourcedTool) Run(ctx context.Context, args string) (ToolResult, error) {
	return ToolResult{Content: "ok"}, nil
}
func (m *mockSourcedTool) RequiresApproval() bool { return false }
func (m *mockSourcedTool) ToolSource() ToolSource { return m.source }

func TestToolRegistry_SortedSchemas(t *testing.T) {
	r := NewToolRegistry()
	// Register in non-alphabetical, mixed-source order
	r.Register(&mockTool{name: "grep"})                                      // local
	r.Register(&mockSourcedTool{name: "browser_click", source: SourceMCP})   // mcp
	r.Register(&mockSourcedTool{name: "web_search", source: SourceGateway})  // gateway
	r.Register(&mockTool{name: "bash"})                                      // local
	r.Register(&mockSourcedTool{name: "browser_type", source: SourceMCP})    // mcp
	r.Register(&mockSourcedTool{name: "alpaca_news", source: SourceGateway}) // gateway
	r.Register(&mockTool{name: "file_read"})                                 // local

	schemas := r.SortedSchemas()
	var names []string
	for _, s := range schemas {
		names = append(names, s.Function.Name)
	}

	expected := []string{
		// local alpha
		"bash", "file_read", "grep",
		// mcp alpha
		"browser_click", "browser_type",
		// gateway alpha
		"alpaca_news", "web_search",
	}
	if len(names) != len(expected) {
		t.Fatalf("got %d schemas, want %d: %v", len(names), len(expected), names)
	}
	for i, want := range expected {
		if names[i] != want {
			t.Errorf("position %d: got %q, want %q (full: %v)", i, names[i], want, names)
			break
		}
	}
}

func TestToolRegistry_SortedNames(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "grep"})
	r.Register(&mockSourcedTool{name: "browser_click", source: SourceMCP})
	r.Register(&mockTool{name: "bash"})

	names := r.SortedNames()
	expected := []string{"bash", "grep", "browser_click"}
	if len(names) != len(expected) {
		t.Fatalf("got %v, want %v", names, expected)
	}
	for i, want := range expected {
		if names[i] != want {
			t.Errorf("position %d: got %q, want %q", i, names[i], want)
		}
	}
}

func TestToolRegistry_SortedSchemas_MCPAdditionDoesNotShiftLocal(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockTool{name: "grep"})
	r.Register(&mockTool{name: "bash"})

	before := r.SortedNames()

	r.Register(&mockSourcedTool{name: "browser_navigate", source: SourceMCP})

	after := r.SortedNames()
	// Local tools should still be in positions 0 and 1 with same order
	for i := 0; i < 2; i++ {
		if before[i] != after[i] {
			t.Errorf("local tool shifted: position %d was %q, now %q", i, before[i], after[i])
		}
	}
}

func TestToolRegistry_SummaryList(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockTool{name: "file_read"})

	summaries := reg.SummaryList()
	if len(summaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(summaries))
	}
	for _, s := range summaries {
		if s.Name == "" {
			t.Error("summary name is empty")
		}
		if s.Description == "" {
			t.Error("summary description is empty")
		}
	}
}

func TestToolRegistry_FullSchemas(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})
	reg.Register(&mockTool{name: "file_read"})
	reg.Register(&mockTool{name: "grep"})

	schemas := reg.FullSchemas([]string{"bash", "file_read"})
	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas, got %d", len(schemas))
	}
	names := map[string]bool{}
	for _, s := range schemas {
		names[s.Function.Name] = true
	}
	if !names["bash"] || !names["file_read"] {
		t.Errorf("expected bash and file_read, got %v", names)
	}
}

func TestToolRegistry_FullSchemas_Nonexistent(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&mockTool{name: "bash"})

	schemas := reg.FullSchemas([]string{"nonexistent"})
	if len(schemas) != 0 {
		t.Fatalf("expected 0 schemas for nonexistent tool, got %d", len(schemas))
	}
}

func TestTurnUsage_CacheTelemetry(t *testing.T) {
	u := &TurnUsage{}

	// Turn 1: cache creation (first turn always creates, no reads)
	u.Add(client.Usage{InputTokens: 5000, CacheCreationTokens: 4000, CacheReadTokens: 0})
	if !u.cacheCapable {
		t.Error("should be cache-capable after seeing CacheCreationTokens > 0")
	}
	if u.cacheMissStreak != 0 {
		t.Errorf("first turn should not count as miss, got streak %d", u.cacheMissStreak)
	}

	// Turn 2: cache hit
	u.Add(client.Usage{InputTokens: 5000, CacheReadTokens: 3500})
	if u.cacheMissStreak != 0 {
		t.Errorf("cache hit should reset streak, got %d", u.cacheMissStreak)
	}

	// Turns 3-5: cache misses
	for i := 0; i < 3; i++ {
		u.Add(client.Usage{InputTokens: 5000, CacheReadTokens: 0})
	}
	if u.cacheMissStreak != 3 {
		t.Errorf("expected miss streak 3, got %d", u.cacheMissStreak)
	}
}

func TestTurnUsage_CacheTelemetry_NonCacheProvider(t *testing.T) {
	u := &TurnUsage{}

	// Provider never returns cache tokens — should not flag as cache-capable
	for i := 0; i < 5; i++ {
		u.Add(client.Usage{InputTokens: 5000})
	}
	if u.cacheCapable {
		t.Error("should not be cache-capable when provider never returns cache tokens")
	}
	if u.cacheMissStreak != 0 {
		t.Errorf("non-cache provider should not accumulate miss streak, got %d", u.cacheMissStreak)
	}
}
