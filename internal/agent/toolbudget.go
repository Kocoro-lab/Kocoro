package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	// directSchemaTokenBudget guards the default Direct-schema workload. The
	// July 2026 built-in registry plus Cloud's web openers measured about 15K
	// estimated tokens; crossing 16K indicates exposure drift that would make
	// every model request materially larger. The override path is to defer an
	// uncommon tool, or deliberately update this baseline with fresh production
	// measurements and the maintained registry test. This remains diagnostic:
	// it never reclassifies tools at runtime.
	directSchemaTokenBudget = 16000

	// charsPerTokenSchema mirrors the context estimator's conservative ratio.
	charsPerTokenSchema = 3.5
)

func estimateToolSchemaTokens(tool Tool) int {
	if tool == nil {
		return 0
	}
	data, err := json.Marshal(buildToolSchema(tool))
	if err != nil {
		return 0
	}
	return int(math.Ceil(float64(len(data)) / charsPerTokenSchema))
}

// estimateSchemaTokens returns a heuristic token count for the named tool
// schemas using compact JSON serialization.
func estimateSchemaTokens(reg *ToolRegistry, names []string) int {
	if reg == nil || len(names) == 0 {
		return 0
	}

	total := 0
	for _, name := range names {
		t, ok := reg.Get(name)
		if !ok {
			continue
		}
		total += estimateToolSchemaTokens(t)
	}
	return total
}

// DirectSchemaTokenBudget exposes the maintained Direct-schema regression
// threshold to the real built-in registry test without duplicating it.
func DirectSchemaTokenBudget() int {
	return directSchemaTokenBudget
}

// EstimateDirectSchemaTokens returns the diagnostic token estimate for every
// effective Direct tool in the registry.
func EstimateDirectSchemaTokens(reg *ToolRegistry) int {
	return directSchemaBudgetReport(reg, directSchemaTokenBudget).Total
}

type schemaBudgetContributor struct {
	Name   string
	Tokens int
}

type schemaBudgetReport struct {
	Total        int
	Budget       int
	Contributors []schemaBudgetContributor
}

func (report schemaBudgetReport) Exceeded() bool {
	return report.Budget > 0 && report.Total > report.Budget
}

// directSchemaBudgetReport measures only effective Direct schemas. It is a
// deterministic diagnostic and never changes exposure.
func directSchemaBudgetReport(reg *ToolRegistry, budget int) schemaBudgetReport {
	report := schemaBudgetReport{Budget: budget}
	if reg == nil {
		return report
	}
	for _, name := range reg.SortedNames() {
		tool, ok := reg.Get(name)
		if !ok || EffectiveToolExposure(tool) != ToolExposureDirect {
			continue
		}
		tokens := estimateToolSchemaTokens(tool)
		report.Total += tokens
		report.Contributors = append(report.Contributors, schemaBudgetContributor{
			Name:   name,
			Tokens: tokens,
		})
	}
	sort.Slice(report.Contributors, func(i, j int) bool {
		if report.Contributors[i].Tokens != report.Contributors[j].Tokens {
			return report.Contributors[i].Tokens > report.Contributors[j].Tokens
		}
		return report.Contributors[i].Name < report.Contributors[j].Name
	})
	return report
}

func formatSchemaBudgetContributors(report schemaBudgetReport, limit int) string {
	if limit <= 0 || limit > len(report.Contributors) {
		limit = len(report.Contributors)
	}
	parts := make([]string, 0, limit+1)
	for _, contributor := range report.Contributors[:limit] {
		parts = append(parts, fmt.Sprintf("%s=%d", contributor.Name, contributor.Tokens))
	}
	if remaining := len(report.Contributors) - limit; remaining > 0 {
		parts = append(parts, fmt.Sprintf("...(+%d)", remaining))
	}
	return strings.Join(parts, ",")
}

// toolSchemaFingerprint hashes schemas, source, and effective exposure in
// deterministic name order. This invalidates warmed schemas when a registry
// refresh changes either callable metadata or the Direct/Deferred boundary.
func toolSchemaFingerprint(reg *ToolRegistry) string {
	if reg == nil {
		return ""
	}

	h := sha256.New()
	for _, name := range reg.SortedNames() {
		t, ok := reg.Get(name)
		if !ok {
			continue
		}
		data, err := json.Marshal(buildToolSchema(t))
		if err != nil {
			continue
		}
		source := SourceLocal
		if sourcer, ok := t.(ToolSourcer); ok {
			source = sourcer.ToolSource()
		}
		namespace := ""
		if provider, ok := t.(ToolSearchNamespaceProvider); ok {
			namespace = provider.ToolSearchNamespace()
		}
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(source))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(namespace))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(EffectiveToolExposure(t)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
