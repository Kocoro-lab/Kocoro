package agents

import (
	"fmt"
	"sync"

	"gopkg.in/yaml.v3"
)

// CategoryEntry is one row in the category registry.
type CategoryEntry struct {
	Code  string          `yaml:"code" json:"code"`
	Label LocalizedString `yaml:"label" json:"label"`
}

// categoryRegistryFile is the on-disk shape of builtin/_category_registry.yaml.
type categoryRegistryFile struct {
	Categories []CategoryEntry `yaml:"categories"`
}

var (
	categoryRegistryOnce sync.Once
	categoryRegistry     map[string]CategoryEntry
	categoryRegistryErr  error
)

// CategoryRegistry returns the loaded map from category code → entry, parsing
// the embedded registry yaml lazily and exactly once per process. The error is
// retained across calls — if the embedded file is corrupt at startup it stays
// corrupt for the lifetime of the daemon, surfaced on every ResolveCategory.
func CategoryRegistry() (map[string]CategoryEntry, error) {
	categoryRegistryOnce.Do(func() {
		data, err := builtinFS.ReadFile("builtin/_category_registry.yaml")
		if err != nil {
			categoryRegistryErr = fmt.Errorf("read category registry: %w", err)
			return
		}
		var file categoryRegistryFile
		if err := yaml.Unmarshal(data, &file); err != nil {
			categoryRegistryErr = fmt.Errorf("parse category registry: %w", err)
			return
		}
		m := make(map[string]CategoryEntry, len(file.Categories))
		for _, e := range file.Categories {
			if e.Code == "" {
				categoryRegistryErr = fmt.Errorf("category registry: empty code")
				return
			}
			if _, dup := m[e.Code]; dup {
				categoryRegistryErr = fmt.Errorf("category registry: duplicate code %q", e.Code)
				return
			}
			m[e.Code] = e
		}
		categoryRegistry = m
	})
	return categoryRegistry, categoryRegistryErr
}

// ResolveCategory looks up a code in the registry and returns the entry.
// An empty code returns (nil, nil) — agents without a category produce a nil
// category field in the API. An unknown code returns an error so callers can
// fail loud at load time rather than silently dropping the field.
func ResolveCategory(code string) (*CategoryEntry, error) {
	if code == "" {
		return nil, nil
	}
	reg, err := CategoryRegistry()
	if err != nil {
		return nil, err
	}
	if entry, ok := reg[code]; ok {
		// Deep-copy: struct copy is shallow over the Label map, and the cache
		// must stay immutable across callers (the daemon's HTTP handler embeds
		// Label directly into the JSON response — a mistaken mutation there
		// would corrupt every subsequent request).
		label := make(LocalizedString, len(entry.Label))
		for k, v := range entry.Label {
			label[k] = v
		}
		return &CategoryEntry{Code: entry.Code, Label: label}, nil
	}
	return nil, fmt.Errorf("unknown category code %q", code)
}
