package tui

import (
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// pickerOption is one selectable row in the generic statePicker dropdown.
type pickerOption struct {
	label string
	desc  string
	value string
}

// pickerKind discriminates which command opened the picker so Enter dispatches
// to the right apply logic. Add a kind per selection command.
type pickerKind int

const (
	pickerKindModel pickerKind = iota
)

// modelTierOptions lists the routing tiers offered by the /model picker.
// config.go validates exactly these three.
func modelTierOptions() []pickerOption {
	return []pickerOption{
		{label: "small", desc: "fastest, lowest cost", value: "small"},
		{label: "medium", desc: "balanced — the default", value: "medium"},
		{label: "large", desc: "most capable", value: "large"},
	}
}

// pickerWrap wraps a picker index around a list of length n (and is safe when
// the list is empty).
func pickerWrap(idx, n int) int {
	if n == 0 {
		return 0
	}
	if idx < 0 {
		return n - 1
	}
	if idx >= n {
		return 0
	}
	return idx
}

// openModelPicker enters the interactive model-tier picker, pre-selecting the
// current tier. Invoked by bare `/model` (typing the value is for power users
// via `/model <tier>`).
func (m *Model) openModelPicker() {
	opts := modelTierOptions()
	m.pickerTitle = "Model tier"
	m.pickerOpts = opts
	m.pickerKind = pickerKindModel
	m.pickerIdx = 0
	for i, o := range opts {
		if o.value == m.cfg.ModelTier {
			m.pickerIdx = i
		}
	}
	m.menuVisible = false // drop any stale slash-menu before switching state
	m.state = statePicker
}

// applyModelTier sets the gateway model tier live (and persists it), mirroring
// the previous `/model <tier>` behavior. Shared by the picker and the direct
// command form.
func (m *Model) applyModelTier(tier string) {
	saveCfg := m.cfg
	if m.baseCfg != nil {
		m.baseCfg.ModelTier = tier
		saveCfg = m.baseCfg
	}
	m.cfg.ModelTier = tier
	m.agentLoop.SetModelTier(tier)
	if err := config.Save(saveCfg); err != nil {
		m.appendOutput(fmt.Sprintf("Model tier: %s (failed to save: %v)", tier, err))
	} else {
		m.appendOutput(fmt.Sprintf("Model tier: %s (saved)", tier))
	}
}
