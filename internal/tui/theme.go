package tui

import "github.com/charmbracelet/lipgloss"

// Centralized semantic color palette.
//
// Every color is an AdaptiveColor so the TUI stays readable on BOTH dark and
// light terminal backgrounds. lipgloss resolves Light/Dark once per program by
// probing the terminal background (falling back to Dark when detection fails,
// which preserves the historical look). Before this file the codebase scattered
// raw lipgloss.Color("243")-style 256-color indices inline; many of those were
// light grays (243/237/252) that vanish to near-invisible on a white terminal.
//
// Rule for new TUI code: reference a token here instead of a bare numeric
// Color(...). theme_test.go enforces this for the migrated semantic colors.
//
// Light = the value shown on a light background (so it must be dark enough to
// read on white); Dark = the value shown on a dark background.
var (
	// colorDim — muted secondary text: tool lines, timestamps, hints, args.
	colorDim = lipgloss.AdaptiveColor{Light: "240", Dark: "245"}

	// colorSecondary — emphasized secondary text: session titles, labels.
	// Brighter/darker than colorDim so it reads as "primary-ish but quiet".
	colorSecondary = lipgloss.AdaptiveColor{Light: "236", Dark: "252"}

	// colorFaint — the subtlest separators (horizontal rule bars). Stays
	// barely-there on both themes instead of a harsh line.
	colorFaint = lipgloss.AdaptiveColor{Light: "252", Dark: "237"}

	// colorSuccess — success / completion (✓), additions.
	colorSuccess = lipgloss.AdaptiveColor{Light: "28", Dark: "42"}

	// colorError — errors, failures (✗), deletions.
	colorError = lipgloss.AdaptiveColor{Light: "160", Dark: "196"}

	// colorWarn — warnings and the approval prompt.
	colorWarn = lipgloss.AdaptiveColor{Light: "130", Dark: "214"}

	// colorAccent — brand accent (the frog green) for headers/emphasis.
	colorAccent = lipgloss.AdaptiveColor{Light: "28", Dark: "76"}

	// colorInfo — informational blue: section headers, session picker.
	colorInfo = lipgloss.AdaptiveColor{Light: "25", Dark: "39"}

	// colorSelect / colorSelectDesc — the highlighted row in a drop list.
	colorSelect     = lipgloss.AdaptiveColor{Light: "25", Dark: "111"}
	colorSelectDesc = lipgloss.AdaptiveColor{Light: "242", Dark: "146"}
)

// Convenience style constructors. These return a fresh style each call (cheap;
// lipgloss styles are value types) so callers can chain .Bold()/.Italic().
func styleDim() lipgloss.Style       { return lipgloss.NewStyle().Foreground(colorDim) }
func styleSecondary() lipgloss.Style { return lipgloss.NewStyle().Foreground(colorSecondary) }
func styleFaint() lipgloss.Style     { return lipgloss.NewStyle().Foreground(colorFaint) }
func styleSuccess() lipgloss.Style   { return lipgloss.NewStyle().Foreground(colorSuccess) }
func styleError() lipgloss.Style     { return lipgloss.NewStyle().Foreground(colorError) }
func styleWarn() lipgloss.Style      { return lipgloss.NewStyle().Foreground(colorWarn) }
func styleAccent() lipgloss.Style    { return lipgloss.NewStyle().Foreground(colorAccent) }
func styleInfo() lipgloss.Style      { return lipgloss.NewStyle().Foreground(colorInfo) }
