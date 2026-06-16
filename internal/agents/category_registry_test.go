package agents

import (
	"testing"
)

func TestCategoryRegistry_Loads(t *testing.T) {
	reg, err := CategoryRegistry()
	if err != nil {
		t.Fatalf("CategoryRegistry: %v", err)
	}
	// Sanity: starter set is 8 entries. If the yaml changes this test wants to
	// know — drift here is usually intentional but worth surfacing.
	if len(reg) < 8 {
		t.Fatalf("registry has %d entries, expected at least 8 (starter set)", len(reg))
	}
	for _, code := range []string{"coding", "writing", "research", "productivity", "data", "design", "personal", "other"} {
		entry, ok := reg[code]
		if !ok {
			t.Errorf("missing starter code %q", code)
			continue
		}
		if entry.Code != code {
			t.Errorf("code mismatch: registry key %q has Code=%q", code, entry.Code)
		}
		// Every starter entry ships three languages.
		for _, locale := range []string{"en", "zh-Hans", "ja"} {
			if v, ok := entry.Label[locale]; !ok || v == "" {
				t.Errorf("code %q missing locale %q", code, locale)
			}
		}
	}
}

func TestResolveCategory_Known(t *testing.T) {
	entry, err := ResolveCategory("coding")
	if err != nil {
		t.Fatalf("ResolveCategory(coding): %v", err)
	}
	if entry == nil {
		t.Fatal("expected non-nil entry for known code")
	}
	if entry.Code != "coding" {
		t.Errorf("Code=%q, want coding", entry.Code)
	}
	if entry.Label["en"] != "Coding" {
		t.Errorf("Label.en=%q, want Coding", entry.Label["en"])
	}
}

func TestResolveCategory_Empty(t *testing.T) {
	entry, err := ResolveCategory("")
	if err != nil {
		t.Fatalf("ResolveCategory(\"\"): %v", err)
	}
	if entry != nil {
		t.Errorf("expected nil entry for empty code, got %+v", entry)
	}
}

func TestResolveCategory_Unknown(t *testing.T) {
	entry, err := ResolveCategory("not-a-real-category-xyz")
	if err == nil {
		t.Fatal("expected error for unknown code, got nil")
	}
	if entry != nil {
		t.Errorf("expected nil entry for unknown code, got %+v", entry)
	}
}

func TestResolveCategory_ReturnsCopy(t *testing.T) {
	// Mutating the returned entry's Label map must not corrupt the cached
	// registry — the next ResolveCategory call should still see the original
	// labels.
	first, err := ResolveCategory("coding")
	if err != nil {
		t.Fatalf("first ResolveCategory: %v", err)
	}
	originalEn := first.Label["en"]
	first.Label["en"] = "MUTATED"

	second, err := ResolveCategory("coding")
	if err != nil {
		t.Fatalf("second ResolveCategory: %v", err)
	}
	if second.Label["en"] == "MUTATED" {
		t.Errorf("registry leaked mutation: second.Label.en=%q (mutation should not have propagated; original was %q)", second.Label["en"], originalEn)
	}
}
