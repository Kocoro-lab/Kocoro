package config

import "strings"

const (
	KoeDefaultOpenAIModel = "gpt-realtime-2.1"
	KoeDefaultQwenModel   = "qwen3.5-omni-flash-realtime"
	KoeDefaultOpenAIVoice = "marin"
	KoeDefaultQwenVoice   = "Tina"
)

var koeRealtimeModels = map[string][]string{
	"openai": {"gpt-realtime-2.1", "gpt-realtime-2.1-mini"},
	"qwen": {
		"qwen3.5-omni-flash-realtime",
		"qwen3.5-omni-plus-realtime",
	},
}

var koeRealtimeVoices = map[string][]string{
	"openai": {"alloy", "ash", "ballad", "coral", "echo", "marin", "sage", "shimmer", "verse", "cedar"},
	"qwen":   {"Tina", "Serena", "Ethan", "Jennifer", "Ryan", "Katerina", "Dylan", "Sunny", "Kiki", "Eric", "Marcus", "Peter", "Rocky", "Li"},
}

func IsValidKoeRealtimeProvider(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto", "openai", "qwen":
		return true
	default:
		return false
	}
}

func IsValidKoeRealtimeModel(provider, model string) bool {
	if strings.TrimSpace(model) == "" {
		return true
	}
	for _, allowed := range koeRealtimeModels[provider] {
		if model == allowed {
			return true
		}
	}
	return false
}

// IsValidKoeRealtimeVoice mirrors the catalog's advertised voices. Empty means
// "use the default" and is always valid; a bad voice otherwise only surfaces as
// a rejected session.update, i.e. a call wedged in "connecting".
func IsValidKoeRealtimeVoice(provider, voice string) bool {
	if strings.TrimSpace(voice) == "" {
		return true
	}
	for _, allowed := range koeRealtimeVoices[provider] {
		if voice == allowed {
			return true
		}
	}
	return false
}

func KoeRealtimeCatalog() map[string]interface{} {
	return map[string]interface{}{
		"provider_modes": []string{"auto", "openai", "qwen"},
		"providers": map[string]interface{}{
			"openai": map[string]interface{}{
				"models":        koeRealtimeModels["openai"],
				"voices":        koeRealtimeVoices["openai"],
				"default_model": KoeDefaultOpenAIModel,
				"default_voice": KoeDefaultOpenAIVoice,
			},
			"qwen": map[string]interface{}{
				"models":        koeRealtimeModels["qwen"],
				"voices":        koeRealtimeVoices["qwen"],
				"default_model": KoeDefaultQwenModel,
				"default_voice": KoeDefaultQwenVoice,
			},
		},
	}
}
