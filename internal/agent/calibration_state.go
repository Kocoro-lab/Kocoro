package agent

import "strings"

// EstOverheadState returns a persistable snapshot of the estimator
// calibration: the current overhead sample, the response model that produced
// it, and the fingerprint of the tool registry the sample was measured under
// (the tools[] schema mass is the dominant term of the sample, so a registry
// change invalidates it). Callers persist the trio in the session checkpoint
// and hand it back to SetEstOverheadState when resuming on a fresh loop.
// With no sample the fingerprint is "" — computing it means JSON-marshalling
// and hashing every tool schema, and this runs on every checkpoint and save.
func (a *AgentLoop) EstOverheadState() (tokens int, model, toolsFingerprint string) {
	tokens = int(a.estOverheadTokens.Load())
	if tokens <= 0 {
		return tokens, "", ""
	}
	if m, ok := a.estOverheadModel.Load().(string); ok {
		model = m
	}
	return tokens, model, toolSchemaFingerprint(a.tools)
}

// ToolsFingerprint exposes the current registry fingerprint for callers that
// need it independently of a calibration sample (e.g. seeding a sample onto
// a loop via SetEstOverheadState).
func (a *AgentLoop) ToolsFingerprint() string {
	return toolSchemaFingerprint(a.tools)
}

// LastSystemPromptEstimate returns the token estimate of the most recent
// Run's final system prompt (0 before the first Run). External compaction
// drivers whose shaped history carries only a placeholder system message must
// add this to their overhead so budgets account for the real prompt.
func (a *AgentLoop) LastSystemPromptEstimate() int {
	return int(a.lastSystemPromptEst.Load())
}

// SetEstOverheadState restores a checkpointed calibration sample onto a fresh
// loop (the daemon builds one AgentLoop per request, so without this every
// resumed Run makes its iteration-0 compaction decisions — proactive
// heuristic, preflight, user truncation — with zero calibration, the exact
// window where the estimate is least trustworthy).
//
// The sample is discarded rather than applied when it can no longer describe
// what this loop will send:
//   - non-positive, or larger than the whole context window (the same sanity
//     clamp the live calibration site applies);
//   - the tool registry changed since the sample was taken (missing or
//     mismatched fingerprint) — schema mass differs;
//   - a specific-model pin is active and the sample's model is not that pin
//     or one of its dated variants — tokenizers and schema overheads differ
//     per model, and a sample without a recorded model cannot prove
//     compatibility with a pin.
//
// Tier-routed loops (no pin) accept a model-tagged sample without local model
// validation: the per-response recalibration corrects any residual skew
// within one turn, and the reactive evidence floor still guards the
// under-estimate direction. Call after SwitchAgent/SetSessionID — both reset
// the calibration.
func (a *AgentLoop) SetEstOverheadState(tokens int, model, toolsFingerprint string) {
	if tokens <= 0 {
		return
	}
	if a.contextWindow > 0 && tokens > a.contextWindow {
		return
	}
	if toolsFingerprint == "" || toolsFingerprint != toolSchemaFingerprint(a.tools) {
		return
	}
	if pin := a.specificModel; pin != "" {
		if model != pin && !strings.HasPrefix(model, pin+"-") {
			return
		}
	}
	a.estOverheadTokens.Store(int64(tokens))
	a.estOverheadModel.Store(model)
}
