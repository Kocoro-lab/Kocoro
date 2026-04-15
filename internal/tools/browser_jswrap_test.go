package tools

import "testing"

func TestWrapJSForEvaluate(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantWrapped bool
	}{
		{"plain expression", "JSON.stringify(document.title)", false},
		{"arrow call", "[...document.querySelectorAll('a')].length", false},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		// Semicolon-terminated expressions and multi-line expression-only
		// scripts must pass through: wrapping them in an IIFE without an
		// explicit `return` would silently change the return value to
		// `undefined`. Only top-level statement keywords (which are syntax
		// errors in expression context) trigger wrapping.
		{"expression with trailing semicolon", "JSON.stringify(document.title);", false},
		{"two expressions semicolon-separated", "foo(); bar()", false},
		{"two expressions newline-separated", "foo()\nbar()", false},
		{"return statement", "return 1", true},
		{"const + return", "const x = 1; return x", true},
		{"let", "let y = 2", true},
		{"var", "var z = 3", true},
		{"function declaration", "function f() { return 1 }", true},
		{"async function", "async function g() { return 1 }", true},
		{"async function with extra whitespace", "async  function g() { return 1 }", true},
		// `async` alone must NOT trigger wrap — `async () => expr` is a
		// valid expression; wrapping it without an explicit `return` in the
		// outer IIFE would silently yield `undefined`.
		{"async arrow expression", "async () => fetch('/x').then(r => r.json())", false},
		{"async arrow without space", "async()=>1", false},
		{"asyncFoo identifier", "asyncFoo", false},
		{"multiline const", "const a = 1\na", true},
		{"leading if", "if (true) { return 1 }", true},
		{"leading try", "try { return 1 } catch (e) {}", true},
		// Identifiers that happen to start with a keyword must NOT be wrapped.
		{"returnValue identifier", "returnValue", false},
		{"constexpr identifier", "constexpr + 1", false},
		// Already wrapped IIFEs pass through unchanged.
		{"user IIFE", "(() => { return 1 })()", false},
		{"user async IIFE", "(async () => { return 1 })()", false},
		{"user function IIFE", "(function() { return 1 })()", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := wrapJSForEvaluate(tc.in)
			wrapped := out != tc.in
			if wrapped != tc.wantWrapped {
				t.Errorf("wrapJSForEvaluate(%q) = %q (wrapped=%v), wantWrapped=%v", tc.in, out, wrapped, tc.wantWrapped)
			}
		})
	}
}
