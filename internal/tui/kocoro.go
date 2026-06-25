package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// kocoroMask is the Kocoro brand swirl as a 16×16 ink bitmap ('1' = mark),
// rasterized offline from the brand vector (KocoroLogo.imageset/kocoro1.svg),
// ink-bounding-box centered. Rendered as 8 terminal lines via the half-block
// technique, filled with the brand pink→peach gradient — replaces the mascot
// in the startup header.
var kocoroMask = [16]string{
	"0000000000000000",
	"0000000011100000",
	"0000000111111000",
	"0000001111111000",
	"0000011111111000",
	"0000111111111000",
	"0001111001011000",
	"0001111011100000",
	"0001111011110000",
	"0001111011111000",
	"0000111101111000",
	"0000011101111000",
	"0000000011111000",
	"0000000111111000",
	"0000000011100000",
	"0000000000000000",
}

const kocoroSize = 16

// Brand gradient endpoints: #F40752 (hot pink) → #F9AB8F (peach).
var (
	kocoroFrom = [3]float64{244, 7, 82}
	kocoroTo   = [3]float64{249, 171, 143}
)

func kocoroInk(row, col int) bool { return kocoroMask[row][col] == '1' }

// kocoroColor returns the brand-gradient color for pixel (col,row). The gradient
// runs diagonally; phase shifts it per startup frame for a subtle shimmer sweep.
func kocoroColor(col, row, frame int) lipgloss.Color {
	t := float64(col+row)/float64(2*(kocoroSize-1)) + float64(frame)/24.0
	t -= float64(int(t)) // wrap into [0,1)
	r := kocoroFrom[0] + (kocoroTo[0]-kocoroFrom[0])*t
	g := kocoroFrom[1] + (kocoroTo[1]-kocoroFrom[1])*t
	b := kocoroFrom[2] + (kocoroTo[2]-kocoroFrom[2])*t
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", int(r), int(g), int(b)))
}

// renderKocoroGrid renders the 16×16 swirl as 8 half-block lines (16 cols each):
// ▀ with fg = upper pixel, bg = lower pixel; ▄ for a lower-only pixel.
// The swirl "draws in" top-to-bottom over the first frames, then holds — a
// shape-level animation (visible regardless of terminal color support) plus the
// brand gradient sweep on top.
func renderKocoroGrid(frame int) []string {
	revealRows := 6 + frame*2 // frame 0: top 6 rows … frame 5+: all 16
	lines := make([]string, kocoroSize/2)
	for i := range lines {
		top, bot := i*2, i*2+1
		var sb strings.Builder
		for col := 0; col < kocoroSize; col++ {
			tInk := kocoroInk(top, col) && top < revealRows
			bInk := kocoroInk(bot, col) && bot < revealRows
			switch {
			case !tInk && !bInk:
				sb.WriteByte(' ')
			case tInk && !bInk:
				sb.WriteString(lipgloss.NewStyle().Foreground(kocoroColor(col, top, frame)).Render("▀"))
			case !tInk && bInk:
				sb.WriteString(lipgloss.NewStyle().Foreground(kocoroColor(col, bot, frame)).Render("▄"))
			default:
				sb.WriteString(lipgloss.NewStyle().
					Foreground(kocoroColor(col, top, frame)).
					Background(kocoroColor(col, bot, frame)).Render("▀"))
			}
		}
		lines[i] = sb.String()
	}
	return lines
}
