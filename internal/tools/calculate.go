package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"strconv"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

const (
	calculateMaxExpressionChars = 512
	calculateMaxASTNodes        = 128
	// Workload: ordinary exact local arithmetic, including scientific notation.
	// Exponents beyond 4096 can turn a few input bytes into tens of millions of
	// digits before the expression/AST limits bind. When this cap binds the tool
	// returns a validation error before allocating the rational. It is purposely
	// not configurable; tasks needing larger symbolic magnitudes should keep
	// scientific notation instead of materializing the integer locally.
	calculateMaxLiteralExponent = 4096
)

// CalculateTool evaluates bounded arithmetic locally. It deliberately accepts
// expressions rather than shell or source code so exact utility work never
// needs a network search or a command runner.
type CalculateTool struct{}

type calculateArgs struct {
	Expression string `json:"expression"`
}

func (t *CalculateTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "calculate",
		Description: "Evaluate arithmetic locally with no network call. Use for exact calculations, including simple arithmetic. " +
			"Supports decimal or integer literals, parentheses, unary +/-, and +, -, *, /, %. Do not use web search for arithmetic this tool can evaluate.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"expression": map[string]any{
					"type":        "string",
					"description": "Arithmetic expression, for example (17 * 19) + 4. Use * and / for multiplication and division.",
				},
			},
		},
		Required: []string{"expression"},
	}
}

func (t *CalculateTool) RequiresApproval() bool            { return false }
func (t *CalculateTool) IsReadOnlyCall(string) bool        { return true }
func (t *CalculateTool) IsConcurrencySafeCall(string) bool { return true }

func (t *CalculateTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	if result, valid := agent.ValidateToolArguments(t.Info(), argsJSON); !valid {
		return result, nil
	}
	var args calculateArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid input: %v", err)), nil
	}
	expression := normalizeArithmeticExpression(strings.TrimSpace(args.Expression))
	if expression == "" {
		return agent.ValidationError("expression must not be empty"), nil
	}
	if len([]rune(expression)) > calculateMaxExpressionChars {
		return agent.ValidationError(fmt.Sprintf("expression exceeds %d characters", calculateMaxExpressionChars)), nil
	}

	parsed, err := parser.ParseExpr(expression)
	if err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arithmetic expression: %v", err)), nil
	}
	nodes := 0
	value, err := evaluateArithmeticExpr(parsed, &nodes)
	if err != nil {
		return agent.ValidationError(err.Error()), nil
	}
	result := map[string]any{
		"expression": expression,
		"result":     formatRational(value),
	}
	if !value.IsInt() {
		result["decimal"] = trimDecimal(value.FloatString(12))
	}
	body, _ := json.Marshal(result)
	return agent.ToolResult{Content: string(body)}, nil
}

func normalizeArithmeticExpression(expression string) string {
	replacer := strings.NewReplacer(
		"×", "*",
		"÷", "/",
		"−", "-",
		"–", "-",
	)
	return replacer.Replace(expression)
}

func evaluateArithmeticExpr(expr ast.Expr, nodes *int) (*big.Rat, error) {
	(*nodes)++
	if *nodes > calculateMaxASTNodes {
		return nil, fmt.Errorf("expression exceeds %d operations", calculateMaxASTNodes)
	}
	switch node := expr.(type) {
	case *ast.ParenExpr:
		return evaluateArithmeticExpr(node.X, nodes)
	case *ast.BasicLit:
		if node.Kind != token.INT && node.Kind != token.FLOAT {
			return nil, fmt.Errorf("unsupported literal %q", node.Value)
		}
		if err := validateNumericLiteralExponent(node.Value); err != nil {
			return nil, err
		}
		value := new(big.Rat)
		if _, ok := value.SetString(strings.ReplaceAll(node.Value, "_", "")); !ok {
			return nil, fmt.Errorf("invalid number %q", node.Value)
		}
		return value, nil
	case *ast.UnaryExpr:
		value, err := evaluateArithmeticExpr(node.X, nodes)
		if err != nil {
			return nil, err
		}
		switch node.Op {
		case token.ADD:
			return value, nil
		case token.SUB:
			return new(big.Rat).Neg(value), nil
		default:
			return nil, fmt.Errorf("unsupported unary operator %q", node.Op)
		}
	case *ast.BinaryExpr:
		left, err := evaluateArithmeticExpr(node.X, nodes)
		if err != nil {
			return nil, err
		}
		right, err := evaluateArithmeticExpr(node.Y, nodes)
		if err != nil {
			return nil, err
		}
		switch node.Op {
		case token.ADD:
			return new(big.Rat).Add(left, right), nil
		case token.SUB:
			return new(big.Rat).Sub(left, right), nil
		case token.MUL:
			return new(big.Rat).Mul(left, right), nil
		case token.QUO:
			if right.Sign() == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			return new(big.Rat).Quo(left, right), nil
		case token.REM:
			if !left.IsInt() || !right.IsInt() {
				return nil, fmt.Errorf("remainder requires integer operands")
			}
			if right.Sign() == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			return new(big.Rat).SetInt(new(big.Int).Rem(left.Num(), right.Num())), nil
		default:
			return nil, fmt.Errorf("unsupported operator %q", node.Op)
		}
	default:
		return nil, fmt.Errorf("only arithmetic literals and operators are allowed")
	}
}

func validateNumericLiteralExponent(literal string) error {
	cleaned := strings.ReplaceAll(literal, "_", "")
	markers := "eE"
	if strings.HasPrefix(cleaned, "0x") || strings.HasPrefix(cleaned, "0X") {
		markers = "pP"
	}
	marker := strings.LastIndexAny(cleaned, markers)
	if marker < 0 {
		return nil
	}
	exponent, err := strconv.ParseInt(cleaned[marker+1:], 10, 64)
	if err != nil || exponent > calculateMaxLiteralExponent || exponent < -calculateMaxLiteralExponent {
		return fmt.Errorf("number exponent exceeds +/- %d", calculateMaxLiteralExponent)
	}
	return nil
}

func formatRational(value *big.Rat) string {
	if value.IsInt() {
		return value.Num().String()
	}
	return value.RatString()
}

func trimDecimal(value string) string {
	value = strings.TrimRight(value, "0")
	value = strings.TrimRight(value, ".")
	if value == "-0" {
		return "0"
	}
	return value
}
