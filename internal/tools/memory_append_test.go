package tools

import (
	"strings"
	"testing"
)

// TestMemoryAppendTool_DescriptionCarriesLanguageDiscipline asserts the
// memory_append tool description tells the model to write entries in English
// by default, even when the conversation is in another language. Without
// this, multilingual MEMORY.md content accumulates and biases later sessions'
// response language under recency (issue #157 self-reinforcement loop).
func TestMemoryAppendTool_DescriptionCarriesLanguageDiscipline(t *testing.T) {
	info := (&MemoryAppendTool{}).Info()

	required := []string{
		"LANGUAGE:",                         // section marker
		"write entries in English by default", // the rule
		"quote the user's words verbatim",   // escape hatch for proper nouns / decisions
		"self-reinforcing loop",             // why it matters
	}
	for _, phrase := range required {
		if !strings.Contains(info.Description, phrase) {
			t.Errorf("memory_append description missing language-discipline phrase %q (issue #157)", phrase)
		}
	}

	// The content parameter description should also carry the rule so a model
	// that only inspects parameter schemas (not the top-level description) sees it.
	contentDesc, ok := paramDescription(info.Parameters, "content")
	if !ok {
		t.Fatal("memory_append must declare a content parameter description")
	}
	if !strings.Contains(contentDesc, "English by default") {
		t.Errorf("content parameter description missing English-default rule, got: %q", contentDesc)
	}
}

// paramDescription pulls a property's description out of an OpenAPI-style
// Parameters map. Returns ("", false) if the path is absent.
func paramDescription(params map[string]any, name string) (string, bool) {
	props, ok := params["properties"].(map[string]any)
	if !ok {
		return "", false
	}
	prop, ok := props[name].(map[string]any)
	if !ok {
		return "", false
	}
	desc, ok := prop["description"].(string)
	return desc, ok
}
