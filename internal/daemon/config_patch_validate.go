package daemon

import (
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// PATCH /config historically deep-merged ANY key into config.yaml, so a typo
// or a misplaced key (the production case: `daemon.endpoint`, which no code
// reads — the real key is top-level `endpoint`) was silently persisted while
// the daemon kept running on defaults. findUnknownConfigField validates the
// PATCH INCREMENT ONLY — the existing yaml is never re-validated, so historic
// configs with stray keys keep loading and saving; they just can't gain NEW
// unknown keys through the API.
//
// The allowed-key tree is derived by reflection from config.Config's
// mapstructure tags (no hand-maintained mirror to drift), with two escape
// hatches:
//   - map-typed fields (mcp_servers, its env, …) have OPEN key names — those
//     are user-defined (server names, env vars) and never "unknown" — while
//     struct-typed map VALUES still validate field-by-field;
//   - configPatchViperOnlyKeys lists dotted keys read straight from viper with
//     no struct field.

// configPatchViperOnlyKeys are PATCHable keys that exist only as viper reads.
// Keep in sync with `viper.Get*("...")` call sites; a miss here surfaces
// loudly as a 400 on a documented knob, not as silent data loss. NOTE:
// `gateway_url` is deliberately ABSENT — writing it re-arms migrateOldConfig,
// which rewrites config.yaml down to four keys and can smuggle a new
// endpoint past the protected-field wall; it is a protected field instead.
var configPatchViperOnlyKeys = map[string]bool{
	"agent.reply_route_index_cap":                       true,
	"agent.system_event_cap":                            true,
	"daemon.mailbox_max_per_route":                      true,
	"daemon.scratch_max_age_days":                       true,
	"memory.sidecar_restart_max":                        true,
	"skills.marketplace.max_attempts":                   true,
	"skills.marketplace.retry_base_backoff_secs":        true,
	"skills.marketplace.clawhub_cache_ttl_secs":         true,
	"skills.marketplace.clawhub_warm_on_startup":        true,
	"skills.marketplace.clawhub_exclude_fill_max_pages": true,
	// internal/sync reads viper directly (no struct section). sync.endpoint
	// is absent here on purpose: it redirects session-content uploads and is
	// a protected field like the top-level endpoint.
	"sync.enabled":                       true,
	"sync.dry_run":                       true,
	"sync.exclude_agents":                true,
	"sync.exclude_sources":               true,
	"sync.batch_max_sessions":            true,
	"sync.batch_max_bytes":               true,
	"sync.single_session_max_bytes":      true,
	"sync.daemon_interval":               true,
	"sync.daemon_startup_delay":          true,
	"sync.lock_timeout":                  true,
	"sync.failed_max_attempts_transient": true,
}

type configPatchKeyNode struct {
	children map[string]*configPatchKeyNode
	// open: map-typed field — child KEY names are user-defined (server names,
	// env var names) and never "unknown".
	open bool
	// valueNode, when non-nil on an open node, validates each child VALUE
	// against a struct schema: mcp_servers.<any-name> is fine, but
	// mcp_servers.<any-name>.commad is a typo the daemon would silently
	// ignore at runtime and must be rejected.
	valueNode *configPatchKeyNode
}

var (
	configPatchKeyTreeOnce sync.Once
	configPatchKeyTree     *configPatchKeyNode
)

func configPatchAllowedKeys() *configPatchKeyNode {
	configPatchKeyTreeOnce.Do(func() {
		configPatchKeyTree = buildConfigPatchKeyNode(reflect.TypeOf(config.Config{}))
		// Graft the viper-only keys into the tree so their PARENTS resolve
		// too: `sync` has no struct field at all, and a leaf-only lookup
		// would reject the section before ever reaching `sync.enabled`.
		for dotted := range configPatchViperOnlyKeys {
			node := configPatchKeyTree
			segments := strings.Split(dotted, ".")
			for i, segment := range segments {
				if node.open {
					break // already accepted wholesale
				}
				if node.children == nil {
					// Grafting under a leaf (a viper-only key nested below a
					// scalar struct field). Without this init the write below
					// panics INSIDE sync.Once — the Once is then spent with a
					// nil tree and every subsequent PATCH nil-derefs.
					node.children = map[string]*configPatchKeyNode{}
				}
				child, ok := node.children[segment]
				if !ok {
					child = &configPatchKeyNode{}
					if i < len(segments)-1 {
						child.children = map[string]*configPatchKeyNode{}
					}
					node.children[segment] = child
				}
				node = child
			}
		}
	})
	return configPatchKeyTree
}

func buildConfigPatchKeyNode(t reflect.Type) *configPatchKeyNode {
	node := &configPatchKeyNode{children: map[string]*configPatchKeyNode{}}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name := field.Tag.Get("mapstructure")
		if name == "" {
			name = strings.Split(field.Tag.Get("yaml"), ",")[0]
		}
		name = strings.Split(name, ",")[0]
		if name == "" || name == "-" {
			// Not persistable — but if the field is still JSON-visible
			// (e.g. mcp_servers.<name>.builtin: mapstructure:"-" with a json
			// tag), a GET /config → PATCH round-trip legitimately echoes it
			// back. Accept it as a harmless leaf instead of 400ing the whole
			// round-trip; the merge writes it to yaml where every loader
			// ignores it.
			if jsonName := strings.Split(field.Tag.Get("json"), ",")[0]; jsonName != "" && jsonName != "-" {
				node.children[jsonName] = &configPatchKeyNode{}
			}
			continue
		}
		ft := field.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Map:
			child := &configPatchKeyNode{open: true}
			et := ft.Elem()
			for et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct && et != reflect.TypeOf(time.Time{}) {
				child.valueNode = buildConfigPatchKeyNode(et)
			}
			node.children[name] = child
		case ft.Kind() == reflect.Struct && ft != reflect.TypeOf(time.Time{}):
			node.children[name] = buildConfigPatchKeyNode(ft)
		default:
			node.children[name] = &configPatchKeyNode{}
		}
	}
	return node
}

// findUnknownConfigField walks the patch against the allowed-key tree and
// returns the first unknown dotted path (deterministic order for stable
// errors). Shape mismatches (scalar where a struct lives) are NOT this
// function's business — only key existence is judged.
func findUnknownConfigField(patch map[string]interface{}) (string, bool) {
	return findUnknownConfigFieldAt(patch, configPatchAllowedKeys(), "")
}

func findUnknownConfigFieldAt(patch map[string]interface{}, node *configPatchKeyNode, prefix string) (string, bool) {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		// null = delete-this-key. Deleting an unknown key is the CLEANUP path
		// for stray/legacy yaml (e.g. removing a misplaced daemon.endpoint) —
		// never reject it.
		if patch[key] == nil {
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if node.open {
			// This level's key names are user-defined (server name, env var);
			// the VALUE still validates against the element schema, if any.
			if node.valueNode != nil {
				if sub, isMap := patch[key].(map[string]interface{}); isMap {
					if unknown, found := findUnknownConfigFieldAt(sub, node.valueNode, path); found {
						return unknown, true
					}
				}
			}
			continue
		}
		child, known := node.children[key]
		if !known {
			return path, true
		}
		if child.open && child.valueNode == nil {
			continue
		}
		if sub, isMap := patch[key].(map[string]interface{}); isMap {
			if unknown, found := findUnknownConfigFieldAt(sub, child, path); found {
				return unknown, true
			}
		}
	}
	return "", false
}
