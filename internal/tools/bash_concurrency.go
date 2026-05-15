package tools

import "strings"

// shellMetacharacters are byte sequences whose presence anywhere in a bash
// command disqualifies it from concurrency-safe execution. The list is
// intentionally broad — false negatives (a safe command flagged unsafe and
// kept serial) are cheap; false positives (an unsafe command run in
// parallel) are dangerous.
//
// CRITICAL: \n and \r are included because bash treats them as command
// separators — without them, a multi-line "command" string would pass the
// metacharacter check, get strings.Fields-split (which eats whitespace
// including newlines), and we'd judge concurrency safety based only on the
// first physical line. Real bash would execute every line.
var shellMetacharacters = []string{
	"&&", "||", ">>", "<<", "$(", "`",
	";", "|", "&", ">", "<", "(", ")", "$",
	"\n", "\r",
}

// readOnlyCommandWhitelist maps the first token of a bash command to a
// predicate that returns true when the rest of the argv is also safe.
// A nil predicate means "any argv is safe for this command".
//
// NOTE: `command` is DELIBERATELY OMITTED — it is a bash builtin that runs
// arbitrary commands bypassing functions/aliases (`command rm` ≡ `rm`).
// `type` IS safe because it only reports how a name would be resolved.
var readOnlyCommandWhitelist = map[string]func(args []string) bool{
	"ls":       nil,
	"pwd":      nil,
	"cat":      noBlockingDevicePath, // reading /dev/random etc. blocks/streams forever.
	"head":     noBlockingDevicePath, // same.
	"tail":     tailArgsSafe,         // rejects -f/-F/--follow plus blocking device paths.
	"wc":       noBlockingDevicePath, // same — wc would never return on /dev/urandom.
	"stat":     noBlockingDevicePath, // stat on /dev/tty would block on input.
	"file":     noBlockingDevicePath, // same.
	"which":    nil,
	"type":     nil,
	"echo":     nil,
	"printenv": nil,
	"env":      envArgsSafe,  // bare `env` is read-only, `env X=1 cmd` isn't.
	"date":     dateArgsSafe, // formatting only, never a clock-set form.
	"whoami":   nil,
	"id":       nil,
	"hostname": hostnameArgsSafe, // bare or read flags only.
	"uname":    nil,
	"true":     nil,
	"false":    nil,
	"git":      gitSubcommandSafe,
	"node":     versionFlagOnly,
	"python":   versionFlagOnly,
	"python3":  versionFlagOnly,
	"go":       goSubcommandSafe,
}

// IsCommandConcurrencySafe reports whether a bash command string is safe to
// run concurrently with other bash invocations in the same agent turn. It is
// intentionally conservative: any shell metacharacter, any unknown leading
// token, or any unrecognized subcommand pattern returns false.
func IsCommandConcurrencySafe(command string) bool {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return false
	}
	for _, m := range shellMetacharacters {
		if strings.Contains(trimmed, m) {
			return false
		}
	}
	tokens := strings.Fields(trimmed)
	if len(tokens) == 0 {
		return false
	}
	predicate, ok := readOnlyCommandWhitelist[tokens[0]]
	if !ok {
		return false
	}
	if predicate == nil {
		return true
	}
	return predicate(tokens[1:])
}

// gitUnconditionallyReadOnlySubcommands lists git subcommands whose every
// argv combination is read-only. Subcommands with both read- and write-
// modes (config, branch, remote, reflog, stash) are handled by explicit
// switch cases in gitSubcommandSafe rather than added here.
var gitUnconditionallyReadOnlySubcommands = map[string]bool{
	"status":    true,
	"diff":      true,
	"log":       true,
	"show":      true,
	"rev-parse": true,
	"describe":  true,
	"blame":     true,
	"ls-files":  true,
	"ls-tree":   true,
}

// gitWriteCapableArgs lists subcommands whose argv can specify a file write or
// external command invocation via --output / --ext-diff. We disqualify any
// argv containing these flags before letting the subcommand fall through to
// the read-only whitelist.
var gitWriteCapableArgs = map[string]bool{
	"log":   true,
	"diff":  true,
	"show":  true,
	"blame": true,
}

// argsContainOutputOrExtDiff scans for `--output[=value]` (writes a file) or
// `--ext-diff` (invokes external command), both of which break read-only
// concurrency-safety even on otherwise-safe git subcommands.
func argsContainOutputOrExtDiff(args []string) bool {
	for _, a := range args {
		if a == "--output" || strings.HasPrefix(a, "--output=") {
			return true
		}
		if a == "--ext-diff" {
			return true
		}
	}
	return false
}

// skipGitGlobalOptions consumes leading global git options (`-C path`,
// `-c key=value`, `-P`, `--no-pager`, `--git-dir`, `--work-tree`,
// `--exec-path`, etc.) and returns the remaining args starting at the
// subcommand. Returns nil if the args are malformed (option consumes a
// value but none follows). Recall is conservative: we only consume options
// known to be safe — anything unrecognized stays in place so the existing
// switch falls through to `false`.
func skipGitGlobalOptions(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := args[0]
		switch {
		case a == "-P", a == "--no-pager", a == "--literal-pathspecs",
			a == "--no-replace-objects", a == "--bare":
			args = args[1:]
		case a == "-C", a == "--git-dir", a == "--work-tree", a == "--exec-path":
			// Two-token form: option + value. Bail if value missing.
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		case a == "-c":
			// `-c key=value` — value is the next token.
			if len(args) < 2 {
				return nil
			}
			args = args[2:]
		case strings.HasPrefix(a, "--git-dir="),
			strings.HasPrefix(a, "--work-tree="),
			strings.HasPrefix(a, "--exec-path="):
			args = args[1:]
		default:
			// Unknown global option — stop consuming. Caller's switch will
			// fall through to the read-only whitelist (which returns false).
			return args
		}
	}
	return args
}

func gitSubcommandSafe(args []string) bool {
	args = skipGitGlobalOptions(args)
	if len(args) == 0 {
		return false
	}
	sub := args[0]
	rest := args[1:]
	// Block --output / --ext-diff on the write-capable read subcommands BEFORE
	// any other recognition — these flags turn an otherwise-read-only invocation
	// into a write or external-command call.
	if gitWriteCapableArgs[sub] && argsContainOutputOrExtDiff(rest) {
		return false
	}
	switch sub {
	case "config":
		// Only --get / --get-all / --get-regexp / --get-urlmatch / --get-color /
		// --list / -l are read-only.
		for _, a := range rest {
			if a == "--get" || a == "--list" || a == "--get-all" || a == "--get-regexp" ||
				a == "--get-urlmatch" || a == "--get-color" || a == "-l" {
				return true
			}
		}
		return false
	case "branch":
		// `git branch` alone or with read-only flags is safe. Any positional
		// arg or write flag (-d/-D/-m/-M/-c/-C) means create/delete/rename.
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") {
				return false
			}
			if a == "-d" || a == "-D" || a == "-m" || a == "-M" || a == "-c" || a == "-C" {
				return false
			}
		}
		return true
	case "remote":
		// Read-only forms: bare `remote`, `-v`, `show <name>`, `get-url <name>`.
		if len(rest) == 0 {
			return true
		}
		if rest[0] == "-v" || rest[0] == "--verbose" {
			return true
		}
		if rest[0] == "show" || rest[0] == "get-url" {
			return true
		}
		return false
	case "reflog":
		// Read-only forms: bare `reflog`, `reflog show ...`. Write forms
		// (expire, delete) explicitly excluded.
		if len(rest) == 0 {
			return true
		}
		return rest[0] == "show"
	case "stash":
		// `stash list` is read; bare `stash` PUSHES. Whitelist only `list`.
		return len(rest) >= 1 && rest[0] == "list"
	}
	return gitUnconditionallyReadOnlySubcommands[sub]
}

// versionFlagOnly returns true only when the sole argument is --version, -V,
// or -v. Used for runtimes whose unconditional invocation enters a REPL or
// runs scripts (node, python).
func versionFlagOnly(args []string) bool {
	if len(args) != 1 {
		return false
	}
	switch args[0] {
	case "--version", "-V", "-v":
		return true
	}
	return false
}

// goSubcommandSafe accepts the read-only `go` subcommands. `env` requires
// extra guarding because `go env -w` / `go env -u` mutate the go env file.
func goSubcommandSafe(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "version", "list", "doc":
		return true
	case "env":
		for _, a := range args[1:] {
			if a == "-w" || a == "-u" || strings.HasPrefix(a, "-w=") || strings.HasPrefix(a, "-u=") {
				return false
			}
		}
		return true
	}
	return false
}

// envArgsSafe accepts only bare `env` (no args). `env VAR=val cmd` runs a
// child command which is exactly what we cannot let through.
func envArgsSafe(args []string) bool {
	return len(args) == 0
}

// dateArgsSafe accepts `date` with no args, or only args that start with `+`
// (POSIX format specifier). Any other argv shape — flag, positional,
// numeric — is rejected. `date -s` / `--set` / BSD `date MMDDhhmm[YY]` all
// can change the system clock when invoked as root, so we require the
// caller to use the format-string form exclusively.
func dateArgsSafe(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "+") {
			return false
		}
	}
	return true
}

// hostnameArgsSafe rejects any positional argument (which would attempt to
// set the hostname). Flags like -s, -f, -d are read-only.
func hostnameArgsSafe(args []string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return false
		}
	}
	return true
}

// containsBlockingDevicePath reports whether any positional arg names a
// system pseudo-device that either blocks forever (/dev/random when entropy
// is depleted, /dev/tty waiting on a terminal) or streams forever
// (/dev/urandom, /dev/zero). Reading those would hold a concurrent slot for
// the entire turn timeout. We intentionally do not try to detect FIFOs in
// /tmp because that is a runtime filesystem property, not a static one.
func containsBlockingDevicePath(args []string) bool {
	for _, a := range args {
		// Skip flag-shaped args; only positionals are file paths here.
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "/dev/random", "/dev/urandom", "/dev/zero":
			return true
		}
		if strings.HasPrefix(a, "/dev/tty") {
			return true
		}
	}
	return false
}

// noBlockingDevicePath wraps containsBlockingDevicePath as a whitelist
// predicate: safe iff no positional arg is a blocking pseudo-device.
func noBlockingDevicePath(args []string) bool {
	return !containsBlockingDevicePath(args)
}

// tailArgsSafe rejects follow-mode (`-f`, `-F`, `--follow[=...]`) which would
// pin a concurrency slot indefinitely, then also rejects reads from blocking
// pseudo-devices via noBlockingDevicePath.
func tailArgsSafe(args []string) bool {
	for _, a := range args {
		if a == "-f" || a == "-F" || a == "--follow" || strings.HasPrefix(a, "--follow=") {
			return false
		}
	}
	return noBlockingDevicePath(args)
}
