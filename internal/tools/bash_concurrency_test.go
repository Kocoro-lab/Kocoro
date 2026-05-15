package tools

import "testing"

func TestIsCommandConcurrencySafe(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		// Safe — top-level read-only commands.
		{"git status", "git status", true},
		{"git status with flag", "git status --porcelain", true},
		{"git diff", "git diff HEAD~1", true},
		{"git log oneline", "git log --oneline -10", true},
		{"git branch", "git branch", true},
		{"git rev-parse", "git rev-parse HEAD", true},
		{"git show", "git show HEAD:README.md", true},
		{"ls", "ls", true},
		{"ls with flags", "ls -la /tmp", true},
		{"pwd", "pwd", true},
		{"cat one file", "cat README.md", true},
		{"head", "head -50 file.txt", true},
		{"tail", "tail -n 100 log.txt", true},
		{"wc", "wc -l file.txt", true},
		{"stat", "stat file.txt", true},
		{"file", "file image.png", true},
		{"which", "which node", true},
		{"type", "type cd", true},
		{"echo literal", "echo hello", true},
		{"node version", "node --version", true},
		{"python version", "python --version", true},
		{"date no args", "date", true},
		{"whoami", "whoami", true},
		{"id", "id", true},
		{"printenv", "printenv", true},

		// Unsafe — write/side-effect commands.
		{"git push", "git push origin main", false},
		{"git commit", "git commit -m 'x'", false},
		{"git checkout", "git checkout main", false},
		{"git pull", "git pull", false},
		{"npm install", "npm install", false},
		{"npm run", "npm run dev", false},
		{"rm", "rm file.txt", false},
		{"mv", "mv a b", false},
		{"mkdir", "mkdir foo", false},
		{"chmod", "chmod +x script.sh", false},
		{"curl", "curl https://example.com", false},
		{"unknown", "fooblargle --help", false},

		// Unsafe — shell metacharacters anywhere.
		{"and-chain", "ls && cat README", false},
		{"or-chain", "ls || cat README", false},
		{"semicolon", "ls; cat README", false},
		{"background-and", "sleep 5 &", false},
		{"pipe", "ls | wc -l", false},
		{"redirect out", "ls > out.txt", false},
		{"redirect in", "cat < in.txt", false},
		{"append", "echo x >> log", false},
		{"command-subst-paren", "echo $(date)", false},
		{"command-subst-backtick", "echo `date`", false},
		{"subshell", "(cd /tmp && ls)", false},
		{"variable-subst", "echo $HOME", false},
		// Critical fail-closed: newline / CR are bash command separators.
		{"newline-separator", "ls\nrm x", false},
		{"crlf-separator", "ls\r\nrm x", false},
		{"bare-cr", "ls\rrm x", false},

		// Unsafe — edge cases.
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"git push hidden in args of safe head", "git status; git push", false},

		// Safe — git config read.
		{"git config get", "git config --get user.email", true},
		// Unsafe — git config write (no --get).
		{"git config set", "git config user.email me@x.com", false},

		// Safe — git remote read forms only.
		{"git remote -v", "git remote -v", true},
		{"git remote show", "git remote show origin", true},
		{"git remote get-url", "git remote get-url origin", true},
		// Unsafe — git remote with write subcommand.
		{"git remote add", "git remote add foo https://x", false},
		{"git remote remove", "git remote remove foo", false},
		{"git remote set-url", "git remote set-url foo https://x", false},

		// Safe — git reflog read forms only.
		{"git reflog bare", "git reflog", true},
		{"git reflog show", "git reflog show HEAD", true},
		// Unsafe — git reflog with write subcommand.
		{"git reflog expire", "git reflog expire --all", false},
		{"git reflog delete", "git reflog delete HEAD@{1}", false},

		// Unsafe — bash `command` builtin can execute arbitrary commands.
		{"command-builtin-rm", "command rm file", false},

		// Unsafe — `go env -w` writes config.
		{"go env -w", "go env -w GOPROXY=https://x", false},
		{"go env -u", "go env -u GOPROXY", false},
		// Safe — `go env` read forms.
		{"go env bare", "go env", true},
		{"go env GOPATH", "go env GOPATH", true},

		// Unsafe — `date` with absolute time arg may set system clock on some systems.
		{"date with numeric arg", "date 010100002026", false},
		// Safe — `date` format-only.
		{"date format", "date +%Y-%m-%d", true},

		// Unsafe — `env VAR=val cmd` runs a child command.
		{"env with assignment", "env FOO=bar ls", false},

		// B1 — git diff/log/show/blame must reject --output (writes a file)
		// and --ext-diff (invokes external command).
		{"git diff --output equals", "git diff --output=/tmp/x HEAD~1", false},
		{"git diff --output space", "git diff --output /tmp/x HEAD~1", false},
		{"git log --output", "git log --output=/tmp/x", false},
		{"git show --output", "git show --output=/tmp/x HEAD", false},
		{"git blame --output", "git blame --output=/tmp/x file.go", false},
		{"git diff --ext-diff", "git diff --ext-diff", false},

		// B2 — `tail -f` / --follow / -F blocks indefinitely (slot DoS).
		{"tail -f", "tail -f /var/log/syslog", false},
		{"tail --follow", "tail --follow file.log", false},
		{"tail -F", "tail -F file.log", false},
		{"tail -n", "tail -n 50 file.log", true},
		{"tail bare", "tail file.log", true},

		// B3 — reads from /dev/random, /dev/urandom, /dev/zero, /dev/tty*
		// block or stream forever.
		{"cat /dev/random", "cat /dev/random", false},
		{"cat /dev/urandom", "cat /dev/urandom", false},
		{"cat /dev/zero", "cat /dev/zero", false},
		{"cat /dev/tty", "cat /dev/tty", false},
		{"head /dev/urandom", "head /dev/urandom", false},
		{"cat /tmp/file", "cat /tmp/file", true},

		// B4 — `git config --get-urlmatch` and `--get-color` are read-only.
		{"git config --get-urlmatch", "git config --get-urlmatch http.https://example.com user", true},
		{"git config --get-color", "git config --get-color color.diff.new", true},

		// B5 — git accepts global options before the subcommand. Recall safe
		// subcommands when prefixed with -C/-c/-P/--no-pager/--git-dir/etc.
		{"git -C path status", "git -C /tmp status", true},
		{"git -C path log", "git -C /tmp log --oneline -5", true},
		{"git --no-pager log", "git --no-pager log", true},
		{"git -P log", "git -P log", true},
		{"git -c user.name=x log", "git -c user.name=x log", true},
		{"git --git-dir path status", "git --git-dir /tmp/.git status", true},
		{"git --git-dir=path status", "git --git-dir=/tmp/.git status", true},
		// Global options must NOT widen scope to unsafe subcommands.
		{"git -C path commit", "git -C /tmp commit -m msg", false},
		{"git -C path push", "git -C /tmp push", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsCommandConcurrencySafe(tc.cmd)
			if got != tc.want {
				t.Errorf("IsCommandConcurrencySafe(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}
