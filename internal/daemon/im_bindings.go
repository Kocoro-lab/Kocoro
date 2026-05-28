package daemon

import (
	"sort"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// formatIMBindings produces the sticky-context "IM bindings:" line value
// from a Cloud channels response. The format is `<agent>=<type>:<channel>`,
// multiple pairs joined by `; `. Bindings are sorted (default-agent first,
// then by agent name, then by type) so the line is byte-stable across
// runs — important for prompt cache hits.
//
// Returns "" when there are no enabled IM bindings; the caller should omit
// the sticky-context line entirely in that case (model correctly infers
// "no bindings" from the line's absence).
//
// Only IM-type bindings (isCloudSource) are included; non-IM channel rows
// (other Cloud-side artefacts) are filtered out.
func formatIMBindings(bindings []client.ChannelBinding) string {
	type pair struct {
		agent string
		typ   string
		name  string
	}
	pairs := make([]pair, 0, len(bindings))
	for _, b := range bindings {
		if !b.Enabled {
			continue
		}
		if !isCloudSource(b.Type) {
			continue
		}
		pairs = append(pairs, pair{
			agent: client.ChannelBindingAgentName(b),
			typ:   b.Type,
			name:  b.Name,
		})
	}
	if len(pairs) == 0 {
		return ""
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		// Default agent (empty string) sorts first; then named agents
		// alphabetically; ties broken by type.
		ai, aj := pairs[i].agent, pairs[j].agent
		if (ai == "") != (aj == "") {
			return ai == ""
		}
		if ai != aj {
			return ai < aj
		}
		return pairs[i].typ < pairs[j].typ
	})

	var sb strings.Builder
	for i, p := range pairs {
		if i > 0 {
			sb.WriteString("; ")
		}
		agent := p.agent
		if agent == "" {
			agent = "default"
		}
		sb.WriteString(agent)
		sb.WriteString("=")
		sb.WriteString(p.typ)
		sb.WriteString(":")
		sb.WriteString(p.name)
	}
	return sb.String()
}
