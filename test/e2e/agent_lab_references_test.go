package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestOffline_AgentLabLaneReferencesResolve walks every script and Go test that
// scripts/agent-lab.sh dispatches to and asserts the target still exists.
//
// Why this is not covered by TestOffline_AgentLabScriptsParse: that test runs
// `bash -n`, which validates syntax only. A lane whose runner invokes a deleted
// script, or whose `-run` regex names a deleted test, is syntactically perfect.
// It passed green while FIVE references were dead — four lane targets removed
// alongside the koe mode classifier, plus one name buried inside a
// `-run '^TestOffline_(A|B|C)$'` alternation, where `go test` silently matches
// nothing and still exits 0. Syntax checking cannot see any of that; only
// resolving the references can.
func TestOffline_AgentLabLaneReferencesResolve(t *testing.T) {
	root := repoRoot()
	script := filepath.Join(root, "scripts", "agent-lab.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read agent-lab.sh: %v", err)
	}
	source := string(body)

	declared := declaredGoTestNames(t, root)
	for _, name := range referencedGoTestNames(source) {
		if !declared[name] {
			t.Errorf("agent-lab.sh dispatches to Go test %q, which no longer exists", name)
		}
	}
	for _, rel := range referencedScriptPaths(source) {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("agent-lab.sh dispatches to %s, which no longer exists: %v", rel, err)
		}
	}
}

var (
	// Matches `-run '^TestFoo$'` and `-run '^TestOffline_(A|B)$'`.
	runFlagPattern = regexp.MustCompile(`-run\s+'\^([^']+)\$'`)
	// One alternation group with a literal prefix/suffix, which is the only
	// shape the lane runners use.
	alternationPattern = regexp.MustCompile(`^([A-Za-z0-9_]*)\(([^)]*)\)([A-Za-z0-9_]*)$`)
	scriptRefPattern   = regexp.MustCompile(`\$repo_dir/(scripts/[A-Za-z0-9._/-]+)`)
	goTestDeclPattern  = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
)

// referencedGoTestNames expands each -run regex into the concrete test names it
// is meant to select. An unexpandable regex is skipped rather than guessed at:
// a false "missing test" would be worse than the narrower coverage.
func referencedGoTestNames(source string) []string {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	for _, match := range runFlagPattern.FindAllStringSubmatch(source, -1) {
		expr := match[1]
		if group := alternationPattern.FindStringSubmatch(expr); group != nil {
			prefix, alternatives, suffix := group[1], group[2], group[3]
			for _, alternative := range strings.Split(alternatives, "|") {
				add(prefix + alternative + suffix)
			}
			continue
		}
		if !strings.ContainsAny(expr, "()|[]*+?.") {
			add(expr)
		}
	}
	return names
}

func referencedScriptPaths(source string) []string {
	seen := map[string]bool{}
	var paths []string
	for _, match := range scriptRefPattern.FindAllStringSubmatch(source, -1) {
		if !seen[match[1]] {
			seen[match[1]] = true
			paths = append(paths, match[1])
		}
	}
	return paths
}

func declaredGoTestNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	declared := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		for _, match := range goTestDeclPattern.FindAllStringSubmatch(string(body), -1) {
			declared[match[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository for test declarations: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("found no Go test declarations; the walk is broken, not the references")
	}
	return declared
}
