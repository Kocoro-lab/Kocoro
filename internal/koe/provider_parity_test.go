//go:build darwin && cgo

package koe

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// PATCH /config validates realtime models against internal/config's allowlist
// while `shan koe` hard-validates against this package's copy at startup. If
// the two drift, Desktop happily persists a model that makes the voice helper
// refuse to launch. This pins the two copies (and the defaults) to each other
// through config's exported catalog.
func TestRealtimeAllowlistsMatchConfigPackage(t *testing.T) {
	catalog := config.KoeRealtimeCatalog()
	providers, ok := catalog["providers"].(map[string]interface{})
	if !ok {
		t.Fatalf("catalog has no providers map: %#v", catalog)
	}
	for provider, models := range realtimeModels {
		spec, ok := providers[string(provider)].(map[string]interface{})
		if !ok {
			t.Fatalf("config catalog missing provider %q", provider)
		}
		catalogModels, ok := spec["models"].([]string)
		if !ok {
			t.Fatalf("config catalog models for %q not []string: %#v", provider, spec["models"])
		}
		if len(catalogModels) != len(models) {
			t.Errorf("provider %q: koe allows %d models, config catalog lists %d", provider, len(models), len(catalogModels))
		}
		for _, model := range catalogModels {
			if _, allowed := models[model]; !allowed {
				t.Errorf("config catalog lists %q for %q but koe.ValidateRealtimeModel rejects it", model, provider)
			}
		}
		for model := range models {
			if !config.IsValidKoeRealtimeModel(string(provider), model) {
				t.Errorf("koe allows %q for %q but config.IsValidKoeRealtimeModel rejects it", model, provider)
			}
		}
	}
	defaults := map[string][2]string{
		"openai model": {DefaultOpenAIRealtimeModel, config.KoeDefaultOpenAIModel},
		"qwen model":   {DefaultQwenRealtimeModel, config.KoeDefaultQwenModel},
		"openai voice": {DefaultOpenAIRealtimeVoice, config.KoeDefaultOpenAIVoice},
		"qwen voice":   {DefaultQwenRealtimeVoice, config.KoeDefaultQwenVoice},
	}
	for name, pair := range defaults {
		if pair[0] != pair[1] {
			t.Errorf("default %s: koe=%q config=%q", name, pair[0], pair[1])
		}
	}
	for _, provider := range []string{"openai", "qwen"} {
		spec := providers[provider].(map[string]interface{})
		voice, _ := spec["default_voice"].(string)
		if !config.IsValidKoeRealtimeVoice(provider, voice) {
			t.Errorf("default voice %q for %q is not in its own voice allowlist", voice, provider)
		}
	}
}
