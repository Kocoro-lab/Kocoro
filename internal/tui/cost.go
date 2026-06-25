package tui

import "fmt"

// friendlyCost formats a USD cost for the turn footer: 2 decimals normally, but
// full precision for sub-cent amounts so a tiny cost doesn't read as $0.00.
func friendlyCost(c float64) string {
	if c > 0 && c < 0.01 {
		return fmt.Sprintf("$%.4f", c)
	}
	return fmt.Sprintf("$%.2f", c)
}
