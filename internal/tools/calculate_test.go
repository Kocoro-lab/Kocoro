package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCalculateTool(t *testing.T) {
	tests := []struct {
		name        string
		expression  string
		wantResult  string
		wantDecimal string
	}{
		{name: "precedence", expression: "17 * 19 + 4", wantResult: "327"},
		{name: "parentheses", expression: "(17 + 19) * 4", wantResult: "144"},
		{name: "unicode operators", expression: "17 × 19", wantResult: "323"},
		{name: "unary", expression: "-5 + +2", wantResult: "-3"},
		{name: "decimal exact", expression: "0.1 + 0.2", wantResult: "3/10", wantDecimal: "0.3"},
		{name: "division", expression: "1 / 3", wantResult: "1/3", wantDecimal: "0.333333333333"},
		{name: "remainder", expression: "17 % 5", wantResult: "2"},
	}
	tool := &CalculateTool{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, _ := json.Marshal(calculateArgs{Expression: tt.expression})
			result, err := tool.Run(context.Background(), string(args))
			if err != nil || result.IsError {
				t.Fatalf("Run err=%v result=%+v", err, result)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(result.Content), &body); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			if body["result"] != tt.wantResult {
				t.Fatalf("result=%v want %s", body["result"], tt.wantResult)
			}
			if tt.wantDecimal != "" && body["decimal"] != tt.wantDecimal {
				t.Fatalf("decimal=%v want %s", body["decimal"], tt.wantDecimal)
			}
		})
	}
}

func TestCalculateToolRejectsUnsafeOrInvalidExpressions(t *testing.T) {
	tests := []string{
		"",
		"1 / 0",
		"1.5 % 1",
		"someFunction(2)",
		"value + 1",
		"2 << 3",
		"1e4097",
		"1e-4097",
		"0x1p4097",
		"1e999999999999999999999",
		strings.Repeat("1+", calculateMaxASTNodes) + "1",
	}
	tool := &CalculateTool{}
	for _, expression := range tests {
		t.Run(expression[:min(len(expression), 32)], func(t *testing.T) {
			args, _ := json.Marshal(calculateArgs{Expression: expression})
			result, err := tool.Run(context.Background(), string(args))
			if err != nil {
				t.Fatalf("Run err=%v", err)
			}
			if !result.IsError || !strings.HasPrefix(result.Content, "[validation error]") {
				t.Fatalf("result=%+v want validation error", result)
			}
		})
	}
}
