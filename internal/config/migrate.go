package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// One-shot config migrations applied at binary startup. Each Migration:
//   - Has a stable, globally-unique ID used as the gate key in
//     ~/.shannon/migrations.json so the migration runs at most once per
//     Shannon dir across daemon / TUI / one-shot processes.
//   - Treats "nothing to migrate" (file absent, value not at the migration
//     target, yaml malformed) as a successful no-op — the marker is still
//     recorded so subsequent launches skip the check entirely.
//   - Backs up the original file before any write so users have a forensics
//     trail if behavior changes on next launch are unexpected.
//
// Migrations are intentionally narrow: each one targets a single byte-level
// rewrite of a specific key. yaml.Marshal would reformat the entire file
// (indentation, comments, key order) which is the opposite of what user
// files should experience during an automatic upgrade.

const (
	migrationsFileName               = "migrations.json"
	migrationsLockName               = "migrations.lock"
	migrationIDContextWindow128To200 = "context_window_128_to_200"
)

// Migration is the contract every one-shot upgrade transform implements.
// Apply receives the resolved shannon dir (caller-validated non-empty) and
// returns (changed, err). A nil err with changed=false means "nothing to
// do here, mark applied"; a non-nil err means "leave the marker unset so
// the next launch retries."
type Migration interface {
	ID() string
	Apply(shannonDir string) (changed bool, err error)
}

type migrationsState struct {
	Applied map[string]migrationRecord `json:"applied"`
}

type migrationRecord struct {
	AppliedAt string `json:"applied_at"`
}

// registeredMigrations is the package-level migration roster. New
// migrations are appended here; the order is the apply order on a fresh
// shannon dir (typically irrelevant since each migration is independent).
var registeredMigrations = []Migration{
	&contextWindow128To200Migration{},
}

// RunPendingMigrations executes any migrations not yet recorded in
// migrations.json. Safe to call from any binary entry point. MUST run
// before viper.ReadInConfig in Load() so the resulting yaml is what
// viper sees on the same launch.
//
// The read-modify-write cycle on migrations.json is guarded by an
// exclusive flock on ~/.shannon/migrations.lock so concurrent launches
// (daemon + TUI + one-shot CLI) cannot race and clobber each other's
// marker writes. Matches the project's Atomic Writes convention; lock
// file is never deleted because concurrent goroutines flock the same
// inode (parity with internal/skills/secrets.go).
func RunPendingMigrations(shannonDir string) {
	if shannonDir == "" {
		return
	}
	lockPath := filepath.Join(shannonDir, migrationsLockName)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kocoro: open migrations lock: %v (skipping migrations)\n", err)
		return
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		fmt.Fprintf(os.Stderr, "kocoro: lock migrations: %v (skipping migrations)\n", err)
		return
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)

	state, _ := loadMigrationsState(shannonDir)
	if state.Applied == nil {
		state.Applied = map[string]migrationRecord{}
	}
	for _, m := range registeredMigrations {
		if _, ok := state.Applied[m.ID()]; ok {
			continue
		}
		if _, err := m.Apply(shannonDir); err != nil {
			fmt.Fprintf(os.Stderr, "kocoro: migration %s failed: %v (will retry on next launch)\n", m.ID(), err)
			continue
		}
		state.Applied[m.ID()] = migrationRecord{AppliedAt: time.Now().UTC().Format(time.RFC3339)}
		if err := saveMigrationsState(shannonDir, state); err != nil {
			// Logged but non-fatal: the migration body already succeeded, so
			// the user state is consistent. The marker miss means we'll
			// re-execute Apply on the next launch — for migrations whose
			// Apply is idempotent (the only kind we accept) this is a
			// benign no-op (probe sees the new value, returns early).
			fmt.Fprintf(os.Stderr, "kocoro: persist migrations.json: %v (migration %s will re-run on next launch)\n", err, m.ID())
		}
	}
}

func loadMigrationsState(shannonDir string) (migrationsState, error) {
	path := filepath.Join(shannonDir, migrationsFileName)
	state := migrationsState{Applied: map[string]migrationRecord{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if len(raw) == 0 {
		return state, nil
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, err
	}
	if state.Applied == nil {
		state.Applied = map[string]migrationRecord{}
	}
	return state, nil
}

func saveMigrationsState(shannonDir string, state migrationsState) error {
	path := filepath.Join(shannonDir, migrationsFileName)
	tmpPath := path + ".tmp"
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmpPath, raw, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// contextWindow128To200Migration rewrites the global agent.context_window
// from the old hardcoded default (128000) to the new default (200000).
// Per-agent yamls under ~/.shannon/agents/<name>/config.yaml are not
// touched — values there are explicit user locks and must be preserved.
type contextWindow128To200Migration struct{}

func (m *contextWindow128To200Migration) ID() string {
	return migrationIDContextWindow128To200
}

// Apply rewrites the global agent.context_window from 128000 to 200000.
//
// "yaml absent / value not 128000 / malformed yaml" each return
// (false, nil) — a no-op success. The caller records the marker, so
// subsequent launches skip this migration entirely. This is intentional
// (avoids re-parsing the yaml on every launch for the rest of the
// install's life). The trade-off: a user who later imports a stale
// config.yaml containing `context_window: 128000` will NOT be migrated
// automatically — the at-rest value stays 128000 forever. Runtime
// auto-detect (maybeAutoAdjustContextWindow) still bumps the in-memory
// contextWindow to the model's true cap on response, so the practical
// impact is a 128K turn-1 threshold on those imported sessions only.
// See PR #126 review #5.
func (m *contextWindow128To200Migration) Apply(shannonDir string) (bool, error) {
	configPath := filepath.Join(shannonDir, "config.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}

	// Capture the user's existing file mode so the rewritten yaml inherits
	// it. SafeWriteConfigAs writes 0644 by default, but users with explicit
	// chmod / umask setups will be surprised if a silent migration tightens
	// the mode to 0600. Backup file uses the same mode for symmetry.
	info, err := os.Stat(configPath)
	if err != nil {
		return false, fmt.Errorf("stat config: %w", err)
	}
	origMode := info.Mode().Perm()

	// Verify via yaml parse: only proceed if the structured value is
	// exactly the migration target. yaml is the source of truth here;
	// the line-based rewrite below is just a formatting-preserving
	// executor that should never run if yaml disagrees.
	var probe struct {
		Agent struct {
			ContextWindow *int `yaml:"context_window"`
		} `yaml:"agent"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		// Malformed yaml: skip silently. viper will surface a real
		// error to the user on the subsequent read in Load().
		return false, nil
	}
	if probe.Agent.ContextWindow == nil || *probe.Agent.ContextWindow != 128000 {
		return false, nil
	}

	// Line-based surgical rewrite. yaml.Marshal would reformat the entire
	// file (indentation, key order, lost comments) which is exactly what
	// must NOT happen to user files. The regex requires an indented
	// `context_window:` line so a hypothetical top-level key with the
	// same name can't be touched.
	newRaw, replaced := replaceIndentedIntLine(raw, "context_window", 128000, 200000)
	if !replaced {
		// yaml said 128000 but the line couldn't be matched (flow-style
		// mapping, multi-line value, etc.). Skip to be safe.
		return false, nil
	}

	backupPath := configPath + ".pre-migrate-" + time.Now().UTC().Format("20060102T150405Z") + ".bak"
	if err := os.WriteFile(backupPath, raw, origMode); err != nil {
		return false, fmt.Errorf("write backup: %w", err)
	}

	tmpPath := configPath + ".migrate.tmp"
	if err := os.WriteFile(tmpPath, newRaw, origMode); err != nil {
		return false, fmt.Errorf("write tmp yaml: %w", err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		_ = os.Remove(tmpPath)
		return false, fmt.Errorf("rename yaml: %w", err)
	}
	return true, nil
}

// replaceIndentedIntLine finds ALL indented `<key>: <oldVal>` lines in raw
// yaml and replaces just the integer with newVal in each. Trailing
// whitespace / comments / EOL are preserved. The leading-whitespace
// requirement guards against accidentally matching a top-level key with
// the same name. Returns (raw, false) when no matching line is found.
//
// "Replace all" rather than "replace first" is intentional: the caller
// (contextWindow128To200Migration.Apply) gates this call on the yaml
// probe seeing agent.context_window == 128000, so we only run when at
// least one match is expected. In the vanishingly unlikely case that
// a user has another nested `context_window: 128000` under some custom
// section, rewriting that too is also semantically correct — same key
// name, same legacy default. Pinning "first only" would require an
// extra ReplaceAllFunc closure for no real safety win. See PR #126
// review #1.
func replaceIndentedIntLine(raw []byte, key string, oldVal, newVal int) ([]byte, bool) {
	pattern := fmt.Sprintf(`(?m)^(\s+%s:\s*)%d(\s*(?:#.*)?)$`, regexp.QuoteMeta(key), oldVal)
	re := regexp.MustCompile(pattern)
	if !re.Match(raw) {
		return raw, false
	}
	replacement := fmt.Sprintf("${1}%d${2}", newVal)
	return []byte(re.ReplaceAllString(string(raw), replacement)), true
}
