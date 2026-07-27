package tools

import (
	"net"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// parseComputerUseHotkeyV1 is the single raw chord parser used by both the
// local execution path and the consequential-risk preflight. It deliberately
// preserves the existing lower-case key/modifier spelling passed to the helper;
// aliases are canonicalized only for policy classification below.
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

// computerUseHotkeyRequiresDestinationAuthorityV1 classifies raw global
// keyboard chords whose effect cannot be made safe by window authority alone.
// The decision is pure and structural: model-authored description/prose and AX
// labels are never inputs. These chords require a trusted element/destination
// authority that the raw hotkey path cannot currently provide, so callers must
// fail closed rather than asking the user to confirm an unbound destination.
func computerUseHotkeyRequiresDestinationAuthorityV1(raw string) bool {
	key, modifiers, ok := parseComputerUseHotkeyV1(raw)
	if !ok {
		return false
	}
	return computerUseKeypressRequiresDestinationAuthorityV1(modifiers, []string{key})
}

// computerUseKeypressRequiresDestinationAuthorityV1 applies the same
// fail-closed policy as raw hotkeys to OpenAI's ordered key sequence. Modifier
// keys are held for the complete sequence, so every non-modifier key must be
// classified against the same chord authority.
func computerUseKeypressRequiresDestinationAuthorityV1(
	modifiers []string,
	keySequence []string,
) bool {
	modifierSet := make(map[string]struct{}, len(modifiers))
	for _, modifier := range modifiers {
		modifierSet[canonicalComputerUseHotkeyTokenV1(modifier)] = struct{}{}
	}
	has := func(modifier string) bool {
		_, exists := modifierSet[modifier]
		return exists
	}

	for _, rawKey := range keySequence {
		key := canonicalComputerUseHotkeyTokenV1(rawKey)
		switch key {
		case "return", "enter", "space":
			return true
		}
		if has("command") {
			switch key {
			case "s", "p", "w", "q", "delete", "backspace":
				return true
			}
		}
		if has("shift") && (key == "delete" || key == "backspace") {
			return true
		}
		if has("command") && has("option") && key == "escape" {
			return true
		}
		if has("command") && has("control") && key == "q" {
			return true
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
	if host == "" {
		return false
	}
	return host == "localhost" ||
		net.ParseIP(host) != nil ||
		strings.Contains(host, ".")
}

func computerUsePlainReturnKeypressV1(args computerUseArgs) bool {
	if len(args.Modifiers) != 0 || len(args.KeySequence) != 1 {
		return false
	}
	key := canonicalComputerUseHotkeyTokenV1(args.KeySequence[0])
	return key == "return" || key == "enter"
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

func (t *ComputerUseTool) consumeLocationNavigationCommitV1(
	args computerUseArgs,
) bool {
	allowed := t.allowsLocationNavigationCommitV1(args)
	t.navigationCommit = nil
	return allowed
}
