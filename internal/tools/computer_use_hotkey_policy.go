package tools

import (
	"net"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// parseComputerUseHotkeyV1 is the single raw chord parser used by the local
// target-bound execution path. It deliberately preserves lower-case
// key/modifier spelling until aliases are canonicalized for the helper wire.
func parseComputerUseHotkeyV1(raw string) (key string, modifiers []string, ok bool) {
	parts := strings.Split(strings.ToLower(raw), "+")
	key = strings.TrimSpace(parts[len(parts)-1])
	if key == "" {
		return "", nil, false
	}
	modifiers = make([]string, 0, len(parts)-1)
	for _, part := range parts[:len(parts)-1] {
		if modifier := strings.TrimSpace(part); modifier != "" {
			modifiers = append(modifiers, modifier)
		}
	}
	return key, modifiers, true
}

func canonicalComputerUseHotkeyTokenV1(token string) string {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "cmd", "meta":
		return "command"
	case "ctrl":
		return "control"
	case "alt":
		return "option"
	case "esc":
		return "escape"
	case "spacebar":
		return "space"
	case "del":
		return "delete"
	default:
		return strings.ToLower(strings.TrimSpace(token))
	}
}

// computerUseKeyboardNeedsExactIntentV1 covers only destination-free keyboard
// actions that can activate or destructively mutate the focused UI. Ordinary
// editing/navigation shortcuts remain window-bound; high-risk activation must
// instead carry executor-authored intent.
func computerUseKeyboardNeedsExactIntentV1(args computerUseArgs) bool {
	var keySequence []string
	modifiers := args.Modifiers
	switch args.Action {
	case "hotkey":
		key, parsedModifiers, ok := parseComputerUseHotkeyV1(args.Keys)
		if !ok {
			return false
		}
		keySequence = []string{key}
		modifiers = parsedModifiers
	case "keypress":
		keySequence = args.KeySequence
	default:
		return false
	}
	modifierSet := make(map[string]struct{}, len(modifiers))
	for _, modifier := range modifiers {
		modifierSet[canonicalComputerUseHotkeyTokenV1(modifier)] = struct{}{}
	}
	has := func(modifier string) bool {
		_, exists := modifierSet[modifier]
		return exists
	}
	for _, rawKey := range keySequence {
		switch canonicalComputerUseHotkeyTokenV1(rawKey) {
		case "return", "enter":
			// Plain Return activates the current control in many apps, while
			// Command/Control-Return is also a common send/publish shortcut.
			// Shift/Option-Return remains an ordinary editing chord.
			if len(modifierSet) == 0 || has("command") || has("control") {
				return true
			}
		case "space":
			// Only plain Space is an activation key. Command-Space and other
			// modified variants are ordinary system/app navigation chords.
			if len(modifierSet) == 0 {
				return true
			}
		case "delete", "backspace":
			if has("shift") || has("command") {
				return true
			}
		}
	}
	return false
}

func computerUseLocationNavigationTextV1(text string) bool {
	if text == "" || text != strings.TrimSpace(text) || !utf8.ValidString(text) ||
		len(text) > 2048 || strings.IndexFunc(text, unicode.IsSpace) >= 0 ||
		strings.IndexFunc(text, unicode.IsControl) >= 0 {
		return false
	}
	candidate := text
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.ParseRequestURI(candidate)
	if err != nil || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := parsed.Hostname()
	return host == "localhost" ||
		net.ParseIP(host) != nil ||
		strings.Contains(host, ".")
}

func computerUsePlainReturnKeypressV1(args computerUseArgs) bool {
	if args.Action != "keypress" ||
		len(args.Modifiers) != 0 ||
		len(args.KeySequence) != 1 {
		return false
	}
	key := canonicalComputerUseHotkeyTokenV1(args.KeySequence[0])
	return key == "return" || key == "enter"
}

func computerUseKeyboardMayUseFocusedWitnessV1(args computerUseArgs) bool {
	var key string
	var modifiers []string
	switch args.Action {
	case "keypress":
		if len(args.KeySequence) != 1 {
			return false
		}
		key = canonicalComputerUseHotkeyTokenV1(args.KeySequence[0])
		modifiers = args.Modifiers
	case "hotkey":
		var ok bool
		key, modifiers, ok = parseComputerUseHotkeyV1(args.Keys)
		if !ok {
			return false
		}
		key = canonicalComputerUseHotkeyTokenV1(key)
	default:
		return false
	}
	if len(modifiers) != 0 {
		return false
	}
	return key == "return" || key == "enter" || key == "space"
}

func openAIComputerLocationFocusShortcutV1(
	action OpenAIComputerActionV1,
) bool {
	if action.Type != OpenAIComputerActionKeypressV1 {
		return false
	}
	modifiers, keys, err := openAIComputerKeySequenceV1(action.Keys)
	if err != nil || len(modifiers) != 1 || len(keys) != 1 {
		return false
	}
	return canonicalComputerUseHotkeyTokenV1(modifiers[0]) == "command" &&
		canonicalComputerUseHotkeyTokenV1(keys[0]) == "l"
}

func computerUseLocationFieldEvidenceV1(
	element computerUseElement,
) bool {
	switch element.Role {
	case "AXTextField", "AXComboBox":
	default:
		return false
	}
	for _, value := range []*string{
		element.Title,
		element.Description,
		element.Desc,
		element.Identifier,
	} {
		if value == nil {
			continue
		}
		label := strings.ToLower(strings.TrimSpace(*value))
		for _, marker := range []string{
			"address", "location", "url", "website", "web address",
		} {
			if strings.Contains(label, marker) {
				return true
			}
		}
	}
	return false
}

func (t *ComputerUseTool) allowsLocationNavigationCommitV1(
	args computerUseArgs,
) bool {
	commit := t.navigationCommit
	return commit != nil &&
		computerUsePlainReturnKeypressV1(args) &&
		t.snapshot != nil &&
		t.snapshot.pid == commit.pid &&
		t.snapshot.bundleID == commit.bundleID &&
		t.snapshot.windowID != nil &&
		*t.snapshot.windowID > 0 &&
		uint64(*t.snapshot.windowID) == uint64(commit.windowID)
}
