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
	// schemaTokenBudget is a diagnostic guard for the directly exposed schema
	// set. It must not reclassify tools: effective exposure is resolved from
	// explicit tool policy and source defaults.
	schemaTokenBudget = 8000

	// charsPerTokenSchema mirrors the context estimator's conservative ratio.
	charsPerTokenSchema = 3.5
)

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
		data, err := json.Marshal(buildToolSchema(t))
		if err != nil {
			continue
		}
		total += int(math.Ceil(float64(len(data)) / charsPerTokenSchema))
	}
	return total
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
		tokens := estimateSchemaTokens(reg, []string{name})
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
