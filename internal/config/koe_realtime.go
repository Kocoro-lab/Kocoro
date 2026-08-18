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
		"qwen3-omni-flash-realtime",
	},
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

func KoeRealtimeCatalog() map[string]interface{} {
	return map[string]interface{}{
		"provider_modes": []string{"auto", "openai", "qwen"},
		"providers": map[string]interface{}{
			"openai": map[string]interface{}{
				"models":        koeRealtimeModels["openai"],
				"voices":        []string{"alloy", "ash", "ballad", "coral", "echo", "marin", "sage", "shimmer", "verse", "cedar"},
				"default_model": KoeDefaultOpenAIModel,
				"default_voice": KoeDefaultOpenAIVoice,
			},
			"qwen": map[string]interface{}{
				"models":        koeRealtimeModels["qwen"],
				"voices":        []string{"Tina", "Serena", "Ethan", "Jennifer", "Ryan", "Katerina", "Dylan", "Sunny", "Kiki", "Eric", "Marcus", "Peter", "Rocky", "Li"},
				"default_model": KoeDefaultQwenModel,
				"default_voice": KoeDefaultQwenVoice,
			},
		},
	}
}
