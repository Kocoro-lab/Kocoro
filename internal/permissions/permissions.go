package permissions

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// PermissionsConfig defines user-configurable permission rules.
type PermissionsConfig struct {
	AllowedDirs       []string `yaml:"allowed_dirs"        json:"allowed_dirs"`
	AllowedCommands   []string `yaml:"allowed_commands"    json:"allowed_commands"`
	DeniedCommands    []string `yaml:"denied_commands"     json:"denied_commands"`
	SensitivePatterns []string `yaml:"sensitive_patterns"  json:"sensitive_patterns"`
	NetworkAllowlist  []string `yaml:"network_allowlist"   json:"network_allowlist"`
}

// prefixDepthTable maps known executables to the number of leading non-flag
// tokens that define a "command family" for token-prefix matching. A smaller
// number is more permissive (one approval covers more variants); a larger
// number is stricter. Unknown executables fall back to defaultPrefixDepth.
//
// Example:
//
//	N=2 for "git" → `git status` and `git push` are different families,
//	                but `git status -uall` and `git status --short` share `git status`.
//	N=2 for "ptengine-cli" → `ptengine-cli config` covers config get/show/list/...,
//	                         `ptengine-cli heatmap` covers heatmap query/filter-values/...
var prefixDepthTable = map[string]int{
	"git":            2,
	"kubectl":        2,
	"docker":         2,
	"docker-compose": 2,
	"npm":            2,
	"yarn":           2,
	"pnpm":           2,
	"cargo":          2,
	"brew":           2,
	"gh":             2,
	"aws":            2,
	"gcloud":         2,
	"terraform":      2,
	"ptengine-cli":   2,
	"agent-browser":  2,
}

// defaultPrefixDepth is used for executables not in prefixDepthTable. Set to
// 3 to be conservative for unfamiliar CLIs (one approval covers fewer variants).
const defaultPrefixDepth = 3

// commandPrefixMatch returns true when any non-safe sub-segment of `cmd`
// shares the same "command family prefix" as any non-safe sub-segment of
// `entry`. Both sides are split into compound segments, default-safe segments
// (e.g. `cd /tmp`, `cat x`) are dropped, redirects are stripped, and the
// remaining segments are compared by their first N non-flag tokens (N is
// determined by the leading executable via prefixDepthTable).
//
// Symmetric: same normalization applies to both inputs, so `git status` does
// NOT match `git push` (they diverge at token 2 of N=2).
//
// Multi-segment cross-product matching: a user authorization like
// `agent-browser click X && agent-browser wait 2 && agent-browser snapshot -i`
// matches future invocations of *any* of those sub-commands individually
// (mirroring the user's intent — the entry expresses approval for the whole
// browser-driver family). This is safe because alwaysAskPrefixes still
// override (e.g. an authorization containing `python -c "..."` does not
// silently allow future `python -c "..."` calls).
//
// This is the fallback for `allowed_commands` matching after literal/glob
// match fails.
func commandPrefixMatch(cmd, entry string) bool {
	cmdSegs := nonSafeSegments(cmd)
	entrySegs := nonSafeSegments(entry)
	if len(cmdSegs) == 0 || len(entrySegs) == 0 {
		return false
	}
	for _, c := range cmdSegs {
		for _, e := range entrySegs {
			if segmentPrefixMatch(c, e) {
				return true
			}
		}
	}
	return false
}

// segmentPrefixMatch compares two single-command segments (after redirect
// strip / default-safe filter) by their first N non-flag tokens.
func segmentPrefixMatch(cmdSeg, entrySeg string) bool {
	cmdFirst := firstToken(cmdSeg)
	entryFirst := firstToken(entrySeg)
	if cmdFirst == "" || entryFirst == "" || cmdFirst != entryFirst {
		return false
	}
	n := prefixDepthFor(cmdFirst)
	cmdPrefix := takeFirstNTokens(cmdSeg, n)
	entryPrefix := takeFirstNTokens(entrySeg, n)
	return cmdPrefix != "" && cmdPrefix == entryPrefix
}

// nonSafeSegments returns all compound sub-segments of cmd that are NOT
// built-in default-safe commands. Redirects are stripped from each.
func nonSafeSegments(cmd string) []string {
	var out []string
	for _, seg := range splitCompoundCommand(cmd) {
		core := stripRedirects(seg)
		if core == "" {
			continue
		}
		if isDefaultSafe(core) {
			continue
		}
		out = append(out, core)
	}
	return out
}

// firstToken returns the leading whitespace-delimited token of cmd, preserving
// quoted regions as single tokens.
func firstToken(cmd string) string {
	tokens := shellTokens(cmd)
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

// takeFirstNTokens returns the first n non-flag tokens of cmd joined by single
// spaces. Flag tokens (starting with '-') are skipped without consuming the
// budget. Returns "" if fewer than n non-flag tokens exist.
func takeFirstNTokens(cmd string, n int) string {
	tokens := shellTokens(cmd)
	var out []string
	for _, t := range tokens {
		if len(out) >= n {
			break
		}
		if len(t) > 0 && t[0] == '-' {
			continue
		}
		out = append(out, t)
	}
	if len(out) < n {
		return ""
	}
	return strings.Join(out, " ")
}

// prefixDepthFor returns the configured prefix depth for an executable name,
// or defaultPrefixDepth if unknown.
func prefixDepthFor(executable string) int {
	if d, ok := prefixDepthTable[executable]; ok {
		return d
	}
	return defaultPrefixDepth
}

// shellTokens splits cmd into whitespace-separated tokens, keeping
// single/double-quoted strings, $(...) and `...` substitutions intact (the
// quotes are kept verbatim in the output token).
func shellTokens(cmd string) []string {
	var tokens []string
	var buf strings.Builder
	var stack []byte
	push := func(c byte) { stack = append(stack, c) }
	pop := func() {
		if n := len(stack); n > 0 {
			stack = stack[:n-1]
		}
	}
	top := func() byte {
		if n := len(stack); n > 0 {
			return stack[n-1]
		}
		return 0
	}
	flush := func() {
		if buf.Len() > 0 {
			tokens = append(tokens, buf.String())
			buf.Reset()
		}
	}
	n := len(cmd)
	for i := 0; i < n; i++ {
		c := cmd[i]
		switch top() {
		case 's':
			buf.WriteByte(c)
			if c == '\'' {
				pop()
			}
			continue
		case 'd':
			if c == '\\' && i+1 < n {
				buf.WriteByte(c)
				buf.WriteByte(cmd[i+1])
				i++
				continue
			}
			buf.WriteByte(c)
			switch {
			case c == '"':
				pop()
			case c == '$' && i+1 < n && cmd[i+1] == '(':
				buf.WriteByte(cmd[i+1])
				i++
				push('p')
			case c == '`':
				push('b')
			}
			continue
		case 'p', 'b':
			buf.WriteByte(c)
			switch c {
			case '\'':
				push('s')
			case '"':
				push('d')
			case ')':
				if top() == 'p' {
					pop()
				}
			case '`':
				if top() == 'b' {
					pop()
				} else {
					push('b')
				}
			case '$':
				if i+1 < n && cmd[i+1] == '(' {
					buf.WriteByte(cmd[i+1])
					i++
					push('p')
				}
			}
			continue
		}
		// top-level
		if c == ' ' || c == '\t' || c == '\n' {
			flush()
			continue
		}
		switch {
		case c == '\'':
			push('s')
		case c == '"':
			push('d')
		case c == '`':
			push('b')
		case c == '$' && i+1 < n && cmd[i+1] == '(':
			buf.WriteByte(c)
			buf.WriteByte(cmd[i+1])
			i++
			push('p')
			continue
		}
		buf.WriteByte(c)
	}
	flush()
	return tokens
}

// alwaysAskPrefixes are high-risk command prefixes that force the approval
// dialog on every invocation, regardless of allowed_commands. These cover:
//   - Arbitrary code execution interpreters (python -c, node -e, bash -c, ...)
//   - Inline JS injection (agent-browser eval)
//   - Supply-chain installers (pip/npm/yarn/cargo/brew install, etc.)
//   - Trailing & (background launch — checked separately via hasTrailingBackground)
//   - rm -rf (covers paths the hard-block list doesn't, e.g. relative deletes)
//
// Match semantics: the command (with redirects stripped and exe basename
// normalized) starts with the prefix followed by space or end. Flag/refspec
// dangers in `git push` are handled separately via isAlwaysAskGitPush because
// they can appear anywhere in the args (`--force-with-lease`, `+main`,
// `:feature`) and may be hidden behind global options (`git -C dir push ...`)
// that defeat fixed-prefix matching. minusMExempt overrides specific safe `-m`
// invocations (e.g. `python3 -m pytest`).
var alwaysAskPrefixes = []string{
	// Arbitrary code execution
	"python -c", "python3 -c",
	"node -e", "node -p", "node --eval", "node --print",
	"ruby -e", "perl -e",
	"bash -c", "sh -c", "zsh -c",
	"python -m", "python3 -m",
	// Browser JS injection
	"agent-browser eval",
	// Generic eval/exec wrappers
	"eval", "exec",
	// Supply chain — installers that fetch and run third-party code (incl.
	// setup hooks, postinstall scripts). Common shorthand variants included:
	//   npm install ↔ npm i
	//   pnpm install ↔ pnpm i ↔ pnpm add
	//   yarn add (yarn bare can also install but is overloaded with `yarn test`
	//   etc., so we don't catch the bare form to avoid false positives)
	//   npx <pkg> downloads + runs an arbitrary package binary in one step.
	"pip install", "pip3 install",
	"npm install", "npm i",
	"yarn add",
	"pnpm install", "pnpm i", "pnpm add",
	"npx",
	"cargo install", "gem install",
	"go install", "brew install",
	// Strong delete (hard-block already covers root-level rm -rf; this catches
	// relative or working-dir deletes which are still dangerous).
	// rm -r (without -f) is intentionally excluded: it still prompts via the
	// normal "ask" path and is less likely to be used for mass deletion than
	// rm -rf. Add it here if the project needs stricter treatment.
	"rm -rf",
}

// gitPushDangerFlags are flag tokens that, if present anywhere in a `git push`
// invocation, indicate a destructive operation and force ask regardless of
// allowed_commands. Token-equality match (`t == flag`) or flag-with-value match
// (`HasPrefix(t, flag+"=")`).
//
// This is necessary because `alwaysAskPrefixes` uses HasPrefix(core, prefix+" "),
// which requires the dangerous flag to be in a fixed position immediately after
// the executable. That misses:
//   - `git push --force-with-lease origin main` (next char after `--force` is `-`)
//   - `git push origin main --force` (flag at end)
//   - `git push --force=ref` (flag with value via =)
//   - `git push --delete origin foo` and `git push -d ...`
//   - `git push --prune origin` (deletes remote refs missing locally)
//   - `git push --prune-tags origin` (alias for --prune in more aggressive form,
//     extending pruning to remote tags missing locally — strictly more
//     destructive than --prune; per git-push(1))
//
// Without this gate, prefix-family matching with N=2 (`git push`) would silently
// auto-allow destructive variants whenever a normal `git push origin main` entry
// exists in allowed_commands.
var gitPushDangerFlags = []string{
	"--force", "-f",
	"--force-with-lease",
	"--force-if-includes",
	"--mirror",
	"--delete", "-d",
	"--prune",
	"--prune-tags",
}

// gitGlobalOptsWithArg are `git` global options (placed BEFORE the subcommand)
// that take a separate-token argument. Used by gitSubcommand to skip them when
// looking for the actual subcommand. Without this, `git -C /tmp push --force`
// would have its subcommand misidentified as `/tmp` instead of `push`, defeating
// the always-ask gate.
//
// The `--opt=value` long form (single token containing `=`) is handled
// generically by gitSubcommand without needing an entry here. Boolean global
// flags like `--no-pager`, `-p`, `--paginate` are also handled generically
// (any flag-shaped token not in this map is assumed to take no separate arg).
var gitGlobalOptsWithArg = map[string]bool{
	"-C":             true,
	"-c":             true,
	"--git-dir":      true,
	"--work-tree":    true,
	"--namespace":    true,
	"--super-prefix": true,
	"--config-env":   true,
	"--exec-path":    true,
	"--list-cmds":    true,
	"--attr-source":  true,
}

var envOptsWithArg = map[string]bool{
	"-u":             true,
	"--unset":        true,
	"-C":             true,
	"--chdir":        true,
	"-S":             true,
	"--split-string": true,
	"-P":             true,
	"--path":         true,
}

var sudoOptsWithArg = map[string]bool{
	"-u":                true,
	"--user":            true,
	"-g":                true,
	"--group":           true,
	"-h":                true,
	"--host":            true,
	"-p":                true,
	"--prompt":          true,
	"-C":                true,
	"--close-from":      true,
	"-D":                true,
	"--chdir":           true,
	"-T":                true,
	"--command-timeout": true,
	"-t":                true,
	"--type":            true,
	"-r":                true,
	"--role":            true,
	"-U":                true,
	"--other-user":      true,
}

func tokenCommandName(t string) string {
	return filepath.Base(unquoteToken(t))
}

func hasAssignmentToken(t string) bool {
	return !strings.HasPrefix(t, "-") && strings.Contains(t, "=")
}

func hasInlineOptionArg(t string, optsWithArg map[string]bool) bool {
	for opt := range optsWithArg {
		if strings.HasPrefix(opt, "--") && strings.HasPrefix(t, opt+"=") {
			return true
		}
	}
	return false
}

func skipOptionTokens(tokens []string, optsWithArg map[string]bool) []string {
	for i := 0; i < len(tokens); i++ {
		t := unquoteToken(tokens[i])
		if t == "--" {
			return tokens[i+1:]
		}
		if hasInlineOptionArg(t, optsWithArg) {
			continue
		}
		if optsWithArg[t] {
			if i+1 >= len(tokens) {
				return nil
			}
			i++
			continue
		}
		if strings.HasPrefix(t, "-") {
			continue
		}
		return tokens[i:]
	}
	return nil
}

func skipEnvAssignments(tokens []string) []string {
	for len(tokens) > 0 && hasAssignmentToken(unquoteToken(tokens[0])) {
		tokens = tokens[1:]
	}
	return tokens
}

func splitEnvString(arg string, rest []string) []string {
	split := shellTokens(unquoteToken(arg))
	return append(split, rest...)
}

func skipEnvWrapper(tokens []string) []string {
	for i := 0; i < len(tokens); i++ {
		t := unquoteToken(tokens[i])
		switch {
		case t == "--":
			return skipEnvAssignments(tokens[i+1:])
		case t == "-S" || t == "--split-string":
			if i+1 >= len(tokens) {
				return nil
			}
			return skipEnvAssignments(splitEnvString(tokens[i+1], tokens[i+2:]))
		case strings.HasPrefix(t, "--split-string="):
			value := strings.TrimPrefix(t, "--split-string=")
			return skipEnvAssignments(splitEnvString(value, tokens[i+1:]))
		case hasInlineOptionArg(t, envOptsWithArg):
			continue
		case envOptsWithArg[t]:
			if i+1 >= len(tokens) {
				return nil
			}
			i++
			continue
		case strings.HasPrefix(t, "-"):
			continue
		default:
			return skipEnvAssignments(tokens[i:])
		}
	}
	return nil
}

func skipCommandWrapper(tokens []string) []string {
	for i := 0; i < len(tokens); i++ {
		t := unquoteToken(tokens[i])
		switch t {
		case "--":
			return tokens[i+1:]
		case "-p":
			continue
		case "-v", "-V":
			return nil
		}
		if strings.HasPrefix(t, "-") {
			return nil
		}
		return tokens[i:]
	}
	return nil
}

func gitInvocationTokens(tokens []string) []string {
	for len(tokens) > 0 {
		switch tokenCommandName(tokens[0]) {
		case "git":
			return tokens
		case "env":
			tokens = skipEnvWrapper(tokens[1:])
		case "command":
			tokens = skipCommandWrapper(tokens[1:])
		case "sudo", "doas":
			tokens = skipOptionTokens(tokens[1:], sudoOptsWithArg)
		case "nohup":
			tokens = tokens[1:]
		case "nice":
			tokens = skipOptionTokens(tokens[1:], map[string]bool{"-n": true, "--adjustment": true})
		case "time":
			tokens = skipOptionTokens(tokens[1:], nil)
		default:
			return nil
		}
	}
	return nil
}

func gitSubcommandIndex(tokens []string) ([]string, int, string) {
	tokens = gitInvocationTokens(tokens)
	if len(tokens) == 0 || tokenCommandName(tokens[0]) != "git" {
		return nil, -1, ""
	}
	for i := 1; i < len(tokens); i++ {
		t := unquoteToken(tokens[i])
		if !strings.HasPrefix(t, "-") {
			return tokens, i, t
		}
		// `--opt=value` long form — single token, skip and continue.
		if strings.Contains(t, "=") {
			continue
		}
		// Option that takes a separate-token argument — skip the option AND
		// its argument. Bounds-check so we don't run off the end.
		if gitGlobalOptsWithArg[t] && i+1 < len(tokens) {
			i++
			continue
		}
		// Boolean flag — just skip it.
	}
	return tokens, -1, ""
}

// gitSubcommand returns the actual git subcommand from a tokenized command,
// skipping global options like `-C <dir>`, `-c <kv>`, `--git-dir=<path>`.
// Returns "" if the command is not a recognized git invocation or no
// subcommand could be located.
//
// Examples:
//
//	["git", "push", ...]                        → "push"
//	["git", "-C", ".", "push", ...]             → "push"
//	["git", "-c", "k=v", "push", ...]           → "push"
//	["git", "--git-dir=/p", "push", ...]        → "push"
//	["git", "--no-pager", "push", ...]          → "push"
//	["git"]                                     → ""
//	["python3", "-c", ...]                      → ""
func gitSubcommand(tokens []string) string {
	_, _, sub := gitSubcommandIndex(tokens)
	return sub
}

// unquoteToken returns the contents of a token with one layer of matching outer
// quotes stripped. Used by isAlwaysAskGitPush so that `'+main'` and `"+main"`
// are detected as destructive refspecs even when quoted by the agent.
func unquoteToken(t string) string {
	if len(t) >= 2 {
		if (t[0] == '"' && t[len(t)-1] == '"') || (t[0] == '\'' && t[len(t)-1] == '\'') {
			return t[1 : len(t)-1]
		}
	}
	return t
}

// isAlwaysAskGitPush returns true if `core` is a git push invocation with any
// destructive flag, destructive refspec, or both. Handles:
//   - `git -C dir push ...`, `git -c k=v push ...`, `git --git-dir=p push ...`
//     (global options before the subcommand) via gitSubcommand.
//   - Destructive flags from gitPushDangerFlags (anywhere in the args).
//   - Destructive refspecs:
//   - `+<refspec>` — force push (overrides non-fast-forward checks). Examples:
//     `git push origin +main`, `git push origin +HEAD:main`.
//   - `:<refname>` — delete remote ref. Example: `git push origin :feature/foo`.
//
// Quoted refspec tokens like `'+main'` / `"+main"` are unquoted before checking
// to prevent trivial bypass via shell quoting.
func isAlwaysAskGitPush(core string) bool {
	tokens := shellTokens(core)
	gitTokens, subIdx, sub := gitSubcommandIndex(tokens)
	if sub != "push" {
		return false
	}
	for _, t := range gitTokens[subIdx+1:] {
		u := unquoteToken(t)
		if u == "" {
			continue
		}
		// Destructive flag.
		for _, f := range gitPushDangerFlags {
			if u == f || strings.HasPrefix(u, f+"=") {
				return true
			}
		}
		// Destructive refspec: leading + (force) or : (delete). These only
		// make sense as refspec args (not flags, not the executable, not the
		// subcommand), so we can match on the first byte directly.
		if u[0] == '+' || u[0] == ':' {
			return true
		}
	}
	return false
}

// minusMExempt are `python -m <module>` (and python3) invocations considered
// safe. They override an `alwaysAskPrefixes` entry of `python -m`/`python3 -m`
// because the inner module is a known well-behaved batch tool.
var minusMExempt = []string{
	"python -m pytest", "python3 -m pytest",
	"python -m http.server", "python3 -m http.server",
	"python -m json.tool", "python3 -m json.tool",
	"python -m py_compile", "python3 -m py_compile",
	"python -m venv", "python3 -m venv",
}

// IsAlwaysAskPrefix reports whether a bash command (possibly compound) contains
// any sub-command that matches a high-risk prefix, dangerous-flag pattern, or
// has a trailing `&` background launch. The internal engine path
// (checkSingleCommand) calls the private isAlwaysAskSingle on each split
// segment; this exported wrapper exists for the daemon-side always_allow
// persistence guard (cmd/daemon.go, internal/daemon/server.go), which receives
// pre-split full command strings and must skip persistence to config.yaml when
// any segment is high-risk.
//
// minusMExempt entries override matching alwaysAskPrefixes (so e.g.
// `python3 -m pytest` is not flagged even though `python3 -m` is in the list).
func IsAlwaysAskPrefix(cmd string) bool {
	for _, sub := range splitCompoundCommand(cmd) {
		if isAlwaysAskSingle(sub) {
			return true
		}
	}
	return false
}

// normalizeExe replaces the leading executable token in cmd with its basename,
// so that full-path invocations like /usr/bin/python3 match the same
// alwaysAskPrefixes entries as the plain name python3.
func normalizeExe(cmd string) string {
	tokens := shellTokens(cmd)
	if len(tokens) == 0 {
		return cmd
	}
	exe := tokens[0]
	base := filepath.Base(exe)
	if base == exe {
		return cmd
	}
	if strings.HasPrefix(cmd, exe) {
		return base + cmd[len(exe):]
	}
	return cmd
}

// isAlwaysAskSingle checks a single (already-split) command segment.
func isAlwaysAskSingle(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return false
	}
	// hasTrailingBackground is checked on the raw (un-stripped) string because
	// stripRedirects consumes trailing & as part of redirect normalization.
	// Swapping these two lines would let "cmd &>/dev/null" slip through.
	if hasTrailingBackground(trimmed) {
		return true
	}
	core := stripRedirects(trimmed)
	if core == "" {
		return false
	}
	// Normalize full-path executables (/usr/bin/python3 → python3) so that
	// absolute-path invocations match the same alwaysAskPrefixes entries.
	core = normalizeExe(core)
	// Exempt list wins (longer prefix beats `python -m`).
	for _, ex := range minusMExempt {
		if strings.HasPrefix(core, ex+" ") || core == ex {
			return false
		}
	}
	for _, prefix := range alwaysAskPrefixes {
		if strings.HasPrefix(core, prefix+" ") || core == prefix {
			return true
		}
	}
	// git push: dangerous-flag/refspec scan handles cases that fixed-prefix
	// matching cannot:
	//   - `git -C dir push --force-with-lease ...` (global option bypass)
	//   - `git push origin +main` / `+HEAD:main` (force-push refspec)
	//   - `git push origin :feature/foo` (delete-ref refspec)
	//   - `git push origin main --force` (flag at end of args)
	if isAlwaysAskGitPush(core) {
		return true
	}
	return false
}

// hardBlockPatterns are always denied and cannot be overridden by config.
var hardBlockPatterns = []string{
	"rm -rf /",
	"rm -rf ~",
	"rm -rf /System",
	"rm -rf /Users",
	"rm -rf /*",
	"> /dev/sd*",
	"> /dev/disk*",
	"mkfs.*",
	"dd if=* of=/dev/*",
	"curl * | sh",
	"curl * | bash",
	"wget * | sh",
	"wget * | bash",
}

// defaultSensitivePatterns are built-in file patterns considered sensitive.
var defaultSensitivePatterns = []string{
	".env",
	".env.*",
	"*.pem",
	"*.key",
	"id_rsa*",
	"id_ed25519*",
	".ssh/config",
	"*.keychain*",
	"tokens.json",
	"credentials.json",
	"*.secrets",
}

// defaultSafeCommands are commands allowed by default without user config.
// These are read-only, informational commands with no side effects.
// Resolution order: hard-block → denied → always-ask → allowed → defaultSafe → ask.
// Users can override with denied_commands if needed.
var defaultSafeCommands = []defaultSafeEntry{
	// --- System info & file inspection ---
	{prefix: "ls"},
	{exact: "pwd"},
	{prefix: "cd"}, // cd in subprocess has no external side effect; cleans up compound prefixes like `cd /tmp && actual-cmd`
	{prefix: "which"},
	{prefix: "whereis"},
	{prefix: "type"},
	{prefix: "echo"},
	{prefix: "cat"},
	{prefix: "head"},
	{prefix: "tail"},
	{prefix: "wc"},
	{prefix: "file"},
	{prefix: "stat"},
	{prefix: "du"},
	{prefix: "df"},
	{exact: "id"},
	{exact: "whoami"},
	{exact: "hostname"},
	{prefix: "uname"},
	{exact: "uptime"},
	{prefix: "date"},
	{prefix: "cal"},
	{exact: "env"}, // exact only — "env CMD" runs a command
	{exact: "printenv"},
	{prefix: "basename"},
	{prefix: "dirname"},
	{prefix: "realpath"},
	{prefix: "readlink"},
	{exact: "true"},
	{exact: "false"},
	{prefix: "seq"},
	{exact: "nproc"},
	{exact: "arch"},
	{exact: "tty"},

	// --- Checksums ---
	{prefix: "md5"},
	{prefix: "md5sum"},
	{prefix: "shasum"},
	{prefix: "sha256sum"},
	{prefix: "cksum"},

	// --- Text processing (stdout only) ---
	{prefix: "grep"},
	{prefix: "egrep"},
	{prefix: "fgrep"},
	{prefix: "rg"},
	{prefix: "ag"},
	{prefix: "ack"},
	{prefix: "sort"},
	{prefix: "uniq"},
	{prefix: "tr"},
	{prefix: "cut"},
	{prefix: "paste"},
	{prefix: "fold"},
	{prefix: "fmt"},
	{prefix: "nl"},
	{prefix: "rev"},
	{prefix: "expand"},
	{prefix: "unexpand"},
	{prefix: "column"},
	{prefix: "comm"},
	{prefix: "diff"},
	{prefix: "colordiff"},
	{prefix: "cmp"},
	{prefix: "strings"},
	{prefix: "od"},
	{prefix: "hexdump"},
	{prefix: "base64"},
	{prefix: "jq"},
	{prefix: "yq"},

	// --- File finding (read-only, find excluded — supports -delete/-exec) ---
	{prefix: "fd"},
	{prefix: "locate"},
	{prefix: "mdfind"},
	{prefix: "tree"},

	// --- Process info ---
	{prefix: "ps"},
	{prefix: "top -l"}, // macOS one-shot mode only
	{prefix: "pgrep"},
	{prefix: "lsof"},
	{exact: "w"},
	{prefix: "who"},
	{prefix: "last"},
	{exact: "groups"},

	// --- Network diagnostics (read-only) ---
	{prefix: "ping"},
	{prefix: "traceroute"},
	{prefix: "tracepath"},
	{prefix: "dig"},
	{prefix: "nslookup"},
	{prefix: "host"},
	{prefix: "ifconfig"},
	{prefix: "ip addr"},
	{prefix: "ip route"},
	{prefix: "ip link"},
	{prefix: "netstat"},
	{prefix: "ss"},
	{prefix: "route"},
	{prefix: "arp"},

	// --- Man / help ---
	{prefix: "man"},
	{prefix: "info"},
	{prefix: "help"},

	// --- Git (read-only subcommands) ---
	{prefix: "git status"},
	{prefix: "git diff"},
	{prefix: "git log"},
	{prefix: "git show"},
	{prefix: "git branch"},
	{prefix: "git tag"},
	{prefix: "git remote"},
	{prefix: "git stash list"},
	{prefix: "git ls-files"},
	{prefix: "git ls-remote"},
	{prefix: "git ls-tree"},
	{prefix: "git rev-parse"},
	{prefix: "git rev-list"},
	{prefix: "git cat-file"},
	{prefix: "git name-rev"},
	{prefix: "git describe"},
	{prefix: "git shortlog"},
	{prefix: "git reflog"},
	{prefix: "git config --get"},
	{prefix: "git config --list"},
	{prefix: "git config -l"},
	{prefix: "git blame"},
	{prefix: "git count-objects"},
	{prefix: "git verify-commit"},
	{prefix: "git verify-tag"},

	// --- Version / info commands ---
	{prefix: "go version"},
	{prefix: "go env"},
	{prefix: "go doc"},
	{prefix: "go list"},
	{prefix: "node --version"},
	{prefix: "python --version"},
	{prefix: "python3 --version"},
	{prefix: "rustc --version"},
	{prefix: "rustup show"},
	{prefix: "rustup which"},
	{prefix: "swift --version"},
	{prefix: "java --version"},
	{prefix: "javac --version"},
	{prefix: "cmake --version"},

	// --- Build & test (dev-trusted, execute project code) ---
	{prefix: "go build"},
	{prefix: "go test"},
	{prefix: "go vet"},
	{prefix: "go mod download"},
	{prefix: "go mod verify"},
	{prefix: "go mod graph"},
	{prefix: "go mod why"},
	{prefix: "make"},
	{prefix: "cargo build"},
	{prefix: "cargo test"},
	{prefix: "cargo check"},
	{prefix: "cargo clippy"},
	{prefix: "cargo bench"},
	{prefix: "cargo doc"},
	{prefix: "npm test"},
	{prefix: "npm run"},
	{prefix: "yarn test"},
	{prefix: "pnpm test"},
	{prefix: "bun test"},
	{prefix: "bun run"},
	{prefix: "deno test"},
	{prefix: "deno lint"},
	{prefix: "deno check"},
	{prefix: "deno info"},
	{prefix: "python -m pytest"},
	{prefix: "python -m py_compile"},
	{prefix: "python -m json.tool"},
	{prefix: "python3 -m pytest"},
	{prefix: "python3 -m py_compile"},
	{prefix: "python3 -m json.tool"},
	{prefix: "pytest"},
	{prefix: "swift build"},
	{prefix: "swift test"},
	{prefix: "mvn test"},
	{prefix: "mvn compile"},
	{prefix: "mvn verify"},
	{prefix: "mvn dependency:tree"},
	{prefix: "gradle test"},
	{prefix: "gradle build"},
	{prefix: "gradle dependencies"},

	// --- Linters (read-only analysis) ---
	{prefix: "eslint"},
	{prefix: "shellcheck"},
	{prefix: "hadolint"},
	{prefix: "yamllint"},
	{prefix: "markdownlint"},
	{prefix: "mypy"},
	{prefix: "pyright"},
	{prefix: "pylint"},
	{prefix: "flake8"},
	{prefix: "bandit"},
	{prefix: "golangci-lint"},
	{prefix: "staticcheck"},
	{prefix: "ruff check"},
	{prefix: "buf lint"},

	// --- Package manager queries (read-only) ---
	{prefix: "brew list"},
	{prefix: "brew info"},
	{prefix: "brew search"},
	{prefix: "brew outdated"},
	{prefix: "brew deps"},
	{prefix: "brew leaves"},
	{prefix: "brew config"},
	{prefix: "brew doctor"},
	{prefix: "brew --version"},
	{prefix: "pip list"},
	{prefix: "pip show"},
	{prefix: "pip freeze"},
	{prefix: "pip check"},
	{prefix: "pip3 list"},
	{prefix: "pip3 show"},
	{prefix: "pip3 freeze"},
	{prefix: "pip3 check"},
	{prefix: "npm list"},
	{prefix: "npm ls"},
	{prefix: "npm outdated"},
	{prefix: "npm view"},
	{prefix: "npm info"},
	{prefix: "npm search"},
	{prefix: "npm audit"},
	{prefix: "npm explain"},
	{prefix: "npm why"},
	{prefix: "yarn list"},
	{prefix: "yarn info"},
	{prefix: "yarn why"},
	{prefix: "cargo tree"},
	{prefix: "cargo metadata"},
	{prefix: "cargo version"},
	{prefix: "cargo search"},
	{prefix: "cargo verify-project"},
	{prefix: "cargo read-manifest"},

	// --- Docker (read-only) ---
	{prefix: "docker ps"},
	{prefix: "docker images"},
	{prefix: "docker image ls"},
	{prefix: "docker inspect"},
	{prefix: "docker logs"},
	{prefix: "docker stats"},
	{prefix: "docker top"},
	{prefix: "docker version"},
	{prefix: "docker info"},
	{prefix: "docker network ls"},
	{prefix: "docker volume ls"},
	{prefix: "docker compose ps"},
	{prefix: "docker compose logs"},
	{prefix: "docker compose config"},

	// --- Kubernetes (read-only) ---
	{prefix: "kubectl get"},
	{prefix: "kubectl describe"},
	{prefix: "kubectl logs"},
	{prefix: "kubectl top"},
	{prefix: "kubectl version"},
	{prefix: "kubectl config view"},
	{prefix: "kubectl config current-context"},
	{prefix: "kubectl config get-contexts"},
	{prefix: "kubectl api-resources"},
	{prefix: "kubectl api-versions"},
	{prefix: "kubectl explain"},
	{prefix: "kubectl cluster-info"},

	// --- Terraform (read-only) ---
	{prefix: "terraform version"},
	{prefix: "terraform validate"},
	{prefix: "terraform state list"},
	{prefix: "terraform state show"},
	{prefix: "terraform output"},
	{prefix: "terraform providers"},
	{prefix: "terraform graph"},
	{prefix: "terraform show"},

	// --- GitHub CLI (read-only) ---
	{prefix: "gh pr list"},
	{prefix: "gh pr view"},
	{prefix: "gh pr status"},
	{prefix: "gh pr diff"},
	{prefix: "gh pr checks"},
	{prefix: "gh issue list"},
	{prefix: "gh issue view"},
	{prefix: "gh issue status"},
	{prefix: "gh repo view"},
	{prefix: "gh release list"},
	{prefix: "gh release view"},
	{prefix: "gh run list"},
	{prefix: "gh run view"},
	{prefix: "gh auth status"},
	{prefix: "gh status"},

	// --- AWS (read-only) ---
	{prefix: "aws sts get-caller-identity"},
	{prefix: "aws s3 ls"},
	{prefix: "aws --version"},

	// --- GCP (read-only) ---
	{prefix: "gcloud version"},
	{prefix: "gcloud info"},
	{prefix: "gcloud config list"},
	{prefix: "gcloud auth list"},
	{prefix: "gcloud projects list"},

	// --- macOS system info ---
	{prefix: "sw_vers"},
	{prefix: "system_profiler"},
	{prefix: "ioreg"},
	{prefix: "diskutil list"},
	{prefix: "defaults read"},
	{prefix: "sysctl"},
	{prefix: "launchctl list"},
}

// defaultSafeEntry defines a safe command match rule.
type defaultSafeEntry struct {
	exact  string // exact match (no arguments allowed)
	prefix string // prefix match (command + any arguments)
}

// isDefaultSafe checks if a command matches the built-in safe defaults.
func isDefaultSafe(cmd string) bool {
	for _, entry := range defaultSafeCommands {
		if entry.exact != "" {
			if cmd == entry.exact {
				return true
			}
		} else if entry.prefix != "" {
			if cmd == entry.prefix || strings.HasPrefix(cmd, entry.prefix+" ") {
				return true
			}
		}
	}
	return false
}

// shellSplitOperators are used to split compound commands.
// Order matters: longer operators must come first.
//
// Bare `&` (background separator) is intentionally NOT in this list — it needs
// context-sensitive handling (must be distinguished from `&&`, `&>` redirect,
// and FD-dup `2>&1`) and the trailing `&` must be preserved on the prior
// segment so hasTrailingBackground catches it. See splitCompoundCommand for
// the inline `&` logic.
var shellSplitOperators = []string{"&&", "||", ";", "|"}

// stripRedirects removes shell I/O redirection operators (and their targets)
// from a single command segment, preserving quoted regions intact. Recognized
// patterns:
//
//   - `>file`, `>>file`, `<file`, optionally with leading FD digit (`2>file`)
//   - `2>&1`, `>&2` etc. (FD duplication)
//   - `&>file`, `&>>file` (combined stdout+stderr)
//   - Trailing single `&` (background launch)
//
// Operators inside single/double quotes or `$(...)` / backtick subs are kept.
// Returns the cleaned command with collapsed internal whitespace.
func stripRedirects(cmd string) string {
	var out strings.Builder
	var stack []byte
	push := func(c byte) { stack = append(stack, c) }
	pop := func() {
		if n := len(stack); n > 0 {
			stack = stack[:n-1]
		}
	}
	top := func() byte {
		if n := len(stack); n > 0 {
			return stack[n-1]
		}
		return 0
	}
	n := len(cmd)
	for i := 0; i < n; i++ {
		c := cmd[i]
		switch top() {
		case 's':
			out.WriteByte(c)
			if c == '\'' {
				pop()
			}
			continue
		case 'd':
			if c == '\\' && i+1 < n {
				out.WriteByte(c)
				out.WriteByte(cmd[i+1])
				i++
				continue
			}
			out.WriteByte(c)
			switch {
			case c == '"':
				pop()
			case c == '$' && i+1 < n && cmd[i+1] == '(':
				out.WriteByte(cmd[i+1])
				i++
				push('p')
			case c == '`':
				push('b')
			}
			continue
		case 'p', 'b':
			out.WriteByte(c)
			switch c {
			case '\'':
				push('s')
			case '"':
				push('d')
			case ')':
				if top() == 'p' {
					pop()
				}
			case '`':
				if top() == 'b' {
					pop()
				} else {
					push('b')
				}
			case '$':
				if i+1 < n && cmd[i+1] == '(' {
					out.WriteByte(cmd[i+1])
					i++
					push('p')
				}
			}
			continue
		}
		// Top-level: detect redirect.
		if span := consumeRedirect(cmd, i); span > 0 {
			i += span - 1
			// Insert a space so adjacent tokens stay separated after stripping.
			if out.Len() > 0 {
				last := out.String()[out.Len()-1]
				if last != ' ' && last != '\t' {
					out.WriteByte(' ')
				}
			}
			continue
		}
		// Top-level: track openers.
		switch {
		case c == '\'':
			push('s')
		case c == '"':
			push('d')
		case c == '`':
			push('b')
		case c == '$' && i+1 < n && cmd[i+1] == '(':
			out.WriteByte(c)
			out.WriteByte(cmd[i+1])
			i++
			push('p')
			continue
		}
		out.WriteByte(c)
	}
	return collapseSpaces(out.String())
}

// consumeRedirect detects whether a redirect starts at cmd[i] (top-level only).
// Returns the number of characters to skip (operator + target), or 0 if no
// redirect starts here.
//
// <<EOF heredoc syntax is not handled: the second < causes the target scan to
// stop immediately (ill-formed → return 0), leaving the << in the string.
// Heredocs in single-line permission checks are extremely rare in practice.
func consumeRedirect(cmd string, i int) int {
	n := len(cmd)
	if i >= n {
		return 0
	}
	start := i
	// Optional leading FD digit (e.g., "2>")
	if cmd[i] >= '0' && cmd[i] <= '9' {
		// must be followed by < or > to count as redirect
		if i+1 >= n || (cmd[i+1] != '<' && cmd[i+1] != '>') {
			return 0
		}
		i++
	}
	// Operator
	switch {
	case i < n && cmd[i] == '&':
		// Only meaningful as redirect if followed by '>' (i.e., &> or &>>).
		// FD-dup like "2>&1" is handled by the > branch below.
		if start != i { // FD digit + & doesn't start a redirect
			return 0
		}
		if i+1 >= n || cmd[i+1] != '>' {
			// Bare & — might be trailing background. Strip only if rest is
			// whitespace through end of string.
			for j := i + 1; j < n; j++ {
				if cmd[j] != ' ' && cmd[j] != '\t' && cmd[j] != '\n' {
					return 0
				}
			}
			return n - start
		}
		i++ // consume '&'
		i++ // consume '>'
		if i < n && cmd[i] == '>' {
			i++ // &>>
		}
	case i < n && (cmd[i] == '<' || cmd[i] == '>'):
		op := cmd[i]
		i++
		if op == '>' && i < n && cmd[i] == '>' {
			i++ // >>
		}
		// FD-dup form like 2>&1 or >&2
		if op == '>' && i < n && cmd[i] == '&' && i+1 < n && cmd[i+1] >= '0' && cmd[i+1] <= '9' {
			i++ // &
			for i < n && cmd[i] >= '0' && cmd[i] <= '9' {
				i++
			}
			return i - start
		}
	default:
		return 0
	}
	// Consume optional whitespace and the target token.
	for i < n && (cmd[i] == ' ' || cmd[i] == '\t') {
		i++
	}
	targetStart := i
	for i < n {
		c := cmd[i]
		if c == ' ' || c == '\t' || c == '\n' {
			break
		}
		if c == '\'' || c == '"' || c == '`' {
			break
		}
		if c == '&' || c == '|' || c == ';' || c == '<' || c == '>' {
			break
		}
		if c == '$' && i+1 < n && cmd[i+1] == '(' {
			break
		}
		i++
	}
	if i == targetStart {
		// No target consumed — ill-formed, don't strip.
		return 0
	}
	return i - start
}

// hasTrailingBackground returns true if a top-level command segment ends with
// a single `&` (background launch). Used by alwaysAskPrefixes to force
// re-prompts on long-running process spawns.
func hasTrailingBackground(cmd string) bool {
	trimmed := strings.TrimRightFunc(cmd, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n'
	})
	if !strings.HasSuffix(trimmed, "&") {
		return false
	}
	// Exclude `&&` (would have been split out earlier, but be defensive).
	if strings.HasSuffix(trimmed, "&&") {
		return false
	}
	// Exclude redirect ops: `&>` or `&>>` end in '>', not '&'.
	return true
}

// collapseSpaces replaces runs of whitespace with a single space and trims.
// Preserves whitespace inside the string semantically (after redirect strip,
// removed redirects leave a single space behind).
func collapseSpaces(s string) string {
	var out strings.Builder
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' {
			if !prevSpace {
				out.WriteByte(' ')
				prevSpace = true
			}
			continue
		}
		out.WriteByte(c)
		prevSpace = false
	}
	return strings.TrimSpace(out.String())
}

// CheckCommand evaluates a bash command against the permission rules.
// Returns decision ("allow", "deny", "ask") and a reason string.
func CheckCommand(cmd string, config *PermissionsConfig) (string, string) {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return "deny", "empty command"
	}

	// 1. Hard-block patterns always deny
	for _, pattern := range hardBlockPatterns {
		if MatchesPattern(trimmed, pattern) {
			return "deny", "matches hard-block pattern: " + pattern
		}
	}

	if config == nil {
		return "ask", "no permission config; requires approval"
	}

	// 2. DeniedCommands patterns
	for _, pattern := range config.DeniedCommands {
		if MatchesPattern(trimmed, pattern) {
			return "deny", "matches denied command pattern: " + pattern
		}
	}

	// 3. Split compound commands and check each sub-command
	subCmds := splitCompoundCommand(trimmed)
	if len(subCmds) > 1 {
		for _, sub := range subCmds {
			decision, reason := checkSingleCommand(sub, config)
			if decision == "deny" {
				return "deny", "sub-command denied: " + reason
			}
		}
		// All sub-commands must be explicitly allowed for the compound to be allowed
		allAllowed := true
		for _, sub := range subCmds {
			decision, _ := checkSingleCommand(sub, config)
			if decision != "allow" {
				allAllowed = false
				break
			}
		}
		if allAllowed {
			return "allow", "all sub-commands allowed"
		}
		return "ask", "compound command requires approval"
	}

	return checkSingleCommand(trimmed, config)
}

// checkSingleCommand checks a single (non-compound) command against config.
func checkSingleCommand(cmd string, config *PermissionsConfig) (string, string) {
	trimmed := strings.TrimSpace(cmd)

	// Re-check hard-block for sub-commands
	for _, pattern := range hardBlockPatterns {
		if MatchesPattern(trimmed, pattern) {
			return "deny", "matches hard-block pattern: " + pattern
		}
	}

	// Re-check denied commands for sub-commands
	for _, pattern := range config.DeniedCommands {
		if MatchesPattern(trimmed, pattern) {
			return "deny", "matches denied command pattern: " + pattern
		}
	}

	// High-risk prefixes always require approval (override allowed_commands).
	if isAlwaysAskSingle(trimmed) {
		return "ask", "high-risk command pattern requires per-call approval"
	}

	// AllowedCommands patterns (user config) — first try literal/glob, then
	// fall back to token-prefix matching for sub-command-family awareness.
	for _, pattern := range config.AllowedCommands {
		if MatchesPattern(trimmed, pattern) {
			return "allow", "matches allowed command pattern: " + pattern
		}
		if commandPrefixMatch(trimmed, pattern) {
			return "allow", "matches allowed command prefix family: " + pattern
		}
	}

	// Built-in safe defaults (lowest-priority allow layer)
	if isDefaultSafe(trimmed) {
		return "allow", "built-in safe command"
	}

	return "ask", "command not in allowed list; requires approval"
}

// splitCompoundCommand splits a command string on top-level shell operators
// (&&, ||, ;, |, and bare `&`), respecting single quotes, double quotes,
// $(...) and backtick command substitutions. Subshell groups `(...)` are
// recursively expanded — their inner segments are emitted as separate parts
// at the top level. Operators inside any quoted/substituted region are
// treated as literal characters.
//
// Examples:
//
//	cmd1 && cmd2                     → ["cmd1", "cmd2"]
//	echo "a && b"                    → [`echo "a && b"`]
//	cmd1; agent-browser eval "a; b"  → ["cmd1", `agent-browser eval "a; b"`]
//	cmd1 & cmd2                      → ["cmd1 &", "cmd2"]   (& kept on prior segment)
//	cmd1 || (python3 -c 'evil')      → ["cmd1", "python3 -c 'evil'"]
//	cmd1 2>&1 & cmd2                 → ["cmd1 2>&1 &", "cmd2"] (FD-dup not split)
//	cmd1 &>/dev/null                 → ["cmd1 &>/dev/null"]   (&> redirect not split)
//
// Bare `&` semantics: a top-level `&` separates commands (background launch
// of the prior). The `&` is preserved on the prior segment so isAlwaysAskSingle's
// hasTrailingBackground check still fires. The splitter does NOT split on `&`
// when it would mistake a redirect (`&>`) or FD-dup (`2>&1`) for a separator.
//
// Subshell `(...)` semantics: the parens themselves are dropped; the inner
// commands are re-tokenized at top level, so a high-risk inner command can be
// independently flagged by isAlwaysAskSingle. Nested groups are supported.
//
// Single quotes preserve everything until the matching `'` (POSIX behavior).
// Double quotes allow `\` escaping and embedded `$(...)` / backtick subs.
// Operators must match exactly at top level (e.g., `&&` and `||` are checked
// before single `|`).
func splitCompoundCommand(cmd string) []string {
	var parts []string
	var buf strings.Builder
	flush := func() {
		s := strings.TrimSpace(buf.String())
		if s != "" {
			parts = append(parts, s)
		}
		buf.Reset()
	}
	// Stack tracks nested contexts:
	//   's' = single quotes
	//   'd' = double quotes
	//   'p' = $(...) substitution
	//   'b' = `...` backtick substitution
	//   'g' = (...) subshell group at top level — splits like top-level
	var stack []byte
	push := func(c byte) { stack = append(stack, c) }
	pop := func() {
		if n := len(stack); n > 0 {
			stack = stack[:n-1]
		}
	}
	top := func() byte {
		if n := len(stack); n > 0 {
			return stack[n-1]
		}
		return 0
	}
	n := len(cmd)
	for i := 0; i < n; i++ {
		c := cmd[i]
		switch top() {
		case 's':
			// Inside '...': only ' closes; no escapes (POSIX).
			buf.WriteByte(c)
			if c == '\'' {
				pop()
			}
			continue
		case 'd':
			// Inside "...": \ escapes next byte; $(/` open subs; " closes.
			if c == '\\' && i+1 < n {
				buf.WriteByte(c)
				buf.WriteByte(cmd[i+1])
				i++
				continue
			}
			buf.WriteByte(c)
			switch {
			case c == '"':
				pop()
			case c == '$' && i+1 < n && cmd[i+1] == '(':
				buf.WriteByte(cmd[i+1])
				i++
				push('p')
			case c == '`':
				push('b')
			}
			continue
		case 'p', 'b':
			// Inside $(...) or `...`: recursive shell context, kept verbatim
			// in the buffer (no top-level splitting inside command substitutions).
			buf.WriteByte(c)
			switch c {
			case '\'':
				push('s')
			case '"':
				push('d')
			case ')':
				if top() == 'p' {
					pop()
				}
			case '`':
				if top() == 'b' {
					pop()
				} else {
					push('b')
				}
			case '$':
				if i+1 < n && cmd[i+1] == '(' {
					buf.WriteByte(cmd[i+1])
					i++
					push('p')
				}
			}
			continue
		}
		// Top-level OR inside 'g' (subshell group). Both share splitting
		// semantics: operators split, ( opens nested group, ) closes group.
		// Closing ) of a subshell group flushes inner content and pops.
		if top() == 'g' && c == ')' {
			flush()
			pop()
			continue
		}
		// Check operators first (longest-match).
		matched := 0
		for _, op := range shellSplitOperators {
			if i+len(op) <= n && cmd[i:i+len(op)] == op {
				matched = len(op)
				break
			}
		}
		if matched > 0 {
			flush()
			i += matched - 1
			continue
		}
		// Bare `&` (background separator). The prior segment keeps the `&` so
		// hasTrailingBackground catches background launches in isAlwaysAskSingle.
		// Excluded:
		//   - `&>` / `&>>` (combined stdout+stderr redirect — next char is '>')
		//   - FD-dup like `2>&1` or `>&2` (prior char is '>')
		// Note: `&&` is already consumed by the operator loop above (longest-match),
		// so a remaining `&` here is unambiguously a background separator.
		if c == '&' &&
			(i+1 >= n || cmd[i+1] != '>') &&
			(i == 0 || cmd[i-1] != '>') {
			buf.WriteByte(c)
			flush()
			continue
		}
		// Top-level openers
		switch {
		case c == '\'':
			push('s')
		case c == '"':
			push('d')
		case c == '`':
			push('b')
		case c == '$' && i+1 < n && cmd[i+1] == '(':
			buf.WriteByte(c)
			buf.WriteByte(cmd[i+1])
			i++
			push('p')
			continue
		case c == '(':
			// Subshell group — flush prior content, push 'g'. The `(` is dropped
			// so the inner commands are tokenized as plain segments.
			flush()
			push('g')
			continue
		}
		buf.WriteByte(c)
	}
	flush()
	return parts
}

// CheckFilePath evaluates a file path for read/write access.
// Uses filepath.EvalSymlinks() to resolve symlinks before checking.
// Returns decision ("allow", "deny", "ask") and a reason string.
func CheckFilePath(path string, action string, config *PermissionsConfig) (string, string) {
	if path == "" {
		return "deny", "empty path"
	}

	// Expand ~ prefix
	expanded := expandHome(path)

	// Resolve symlinks to get the real path
	realPath, err := filepath.EvalSymlinks(expanded)
	if err != nil {
		// If the file doesn't exist yet, use the cleaned expanded path
		realPath = filepath.Clean(expanded)
	}

	// Check sensitive file patterns
	if IsSensitiveFile(filepath.Base(realPath)) {
		if action == "read" {
			return "ask", "sensitive file requires approval for read: " + filepath.Base(realPath)
		}
		return "ask", "sensitive file requires approval: " + filepath.Base(realPath)
	}

	if config == nil {
		return "ask", "no permission config; requires approval"
	}

	// Check if path is within allowed_dirs
	inAllowed := false
	for _, dir := range config.AllowedDirs {
		expandedDir := expandHome(dir)
		absDir, err := filepath.Abs(expandedDir)
		if err != nil {
			continue
		}
		absPath, err := filepath.Abs(realPath)
		if err != nil {
			continue
		}
		if isSubPath(absPath, absDir) {
			inAllowed = true
			break
		}
	}

	if inAllowed && action == "read" {
		return "allow", "path within allowed directory"
	}

	if action == "write" {
		return "ask", "write operations always require approval"
	}

	if inAllowed {
		return "allow", "path within allowed directory"
	}

	return "ask", "path not in allowed directories; requires approval"
}

// CheckNetworkEgress evaluates an HTTP request URL against the network allowlist.
// localhost/127.0.0.1 are always allowed.
// Returns decision ("allow", "deny", "ask") and a reason string.
func CheckNetworkEgress(rawURL string, config *PermissionsConfig) (string, string) {
	if rawURL == "" {
		return "deny", "empty URL"
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "deny", "malformed URL: " + err.Error()
	}

	host := parsed.Hostname()

	// localhost and 127.0.0.1 are always allowed
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return "allow", "localhost always allowed"
	}

	if config == nil {
		return "ask", "no permission config; requires approval"
	}

	// Check network allowlist
	for _, allowed := range config.NetworkAllowlist {
		if host == allowed {
			return "allow", "host in network allowlist"
		}
		// Support wildcard subdomain matching: *.example.com
		if strings.HasPrefix(allowed, "*.") {
			suffix := allowed[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) || host == allowed[2:] {
				return "allow", "host matches network allowlist pattern: " + allowed
			}
		}
	}

	return "ask", "host not in network allowlist; requires approval"
}

// CheckToolCall evaluates a tool call against permission rules based on tool name.
// Returns decision ("allow", "deny", "ask", "") and a reason string.
// An empty decision means the tool is not handled by the permissions engine.
func CheckToolCall(toolName, argsJSON string, config *PermissionsConfig) (string, string) {
	switch toolName {
	case "bash":
		cmd := ExtractField(argsJSON, "command")
		return CheckCommand(cmd, config)
	case "file_read":
		path := ExtractField(argsJSON, "path")
		return CheckFilePath(path, "read", config)
	case "file_write", "file_edit":
		path := ExtractField(argsJSON, "path")
		return CheckFilePath(path, "write", config)
	case "glob", "grep":
		path := ExtractField(argsJSON, "path")
		if path == "" {
			path = ExtractField(argsJSON, "pattern")
		}
		return CheckFilePath(path, "read", config)
	case "directory_list":
		path := ExtractField(argsJSON, "path")
		if path == "" {
			path = "."
		}
		return CheckFilePath(path, "read", config)
	case "http":
		url := ExtractField(argsJSON, "url")
		return CheckNetworkEgress(url, config)
	}
	return "", ""
}

// IsHardBlocked checks if a command matches any hard-block pattern.
func IsHardBlocked(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	for _, pattern := range hardBlockPatterns {
		if MatchesPattern(trimmed, pattern) {
			return true
		}
	}
	return false
}

// ExtractField extracts a string field from a JSON args string.
func ExtractField(argsJSON string, field string) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return ""
	}
	if v, ok := m[field]; ok {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// IsSensitiveFile checks if a filename matches known sensitive file patterns.
func IsSensitiveFile(filename string) bool {
	if filename == "" {
		return false
	}
	for _, pattern := range defaultSensitivePatterns {
		if MatchesPattern(filename, pattern) {
			return true
		}
	}
	return false
}

// IsSensitiveFileWithConfig checks against both default and user-configured sensitive patterns.
func IsSensitiveFileWithConfig(filename string, config *PermissionsConfig) bool {
	if IsSensitiveFile(filename) {
		return true
	}
	if config == nil {
		return false
	}
	for _, pattern := range config.SensitivePatterns {
		if MatchesPattern(filename, pattern) {
			return true
		}
	}
	return false
}

// MatchesPattern checks if a string matches a glob-like pattern.
// Supports * as a wildcard matching any sequence of characters.
func MatchesPattern(s string, pattern string) bool {
	return matchGlob(s, pattern)
}

// matchGlob implements simple glob matching with * wildcards.
func matchGlob(s, pattern string) bool {
	// Use a two-pointer approach for * wildcard matching
	si, pi := 0, 0
	starIdx, matchIdx := -1, 0

	for si < len(s) {
		if pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]) {
			si++
			pi++
		} else if pi < len(pattern) && pattern[pi] == '*' {
			starIdx = pi
			matchIdx = si
			pi++
		} else if starIdx != -1 {
			pi = starIdx + 1
			matchIdx++
			si = matchIdx
		} else {
			return false
		}
	}

	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}

	return pi == len(pattern)
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

// isSubPath checks if path is within or equal to dir.
func isSubPath(path, dir string) bool {
	// Normalize paths
	path = filepath.Clean(path)
	dir = filepath.Clean(dir)

	if path == dir {
		return true
	}

	// Ensure dir ends with separator for prefix matching
	dirWithSep := dir + string(filepath.Separator)
	return strings.HasPrefix(path, dirWithSep)
}
