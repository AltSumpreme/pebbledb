package executor

import (
	"fmt"
	"math"
	"math/big"
	"pebbledb/sql/binder"
	"pebbledb/types"
	"strings"
)

func evaluate(expression *binder.Expression, row map[cellKey]types.Value) (types.Value, error) {
	if expression == nil {
		return types.Value{}, fmt.Errorf("sql executor: cannot evaluate nil expression")
	}
	switch expression.Kind {
	case binder.LiteralExpression:
		return expression.Literal, nil
	case binder.ColumnExpression:
		value, exists := row[cellKey{tableID: expression.TableID, columnID: expression.ColumnID}]
		if !exists {
			return types.Value{}, fmt.Errorf("sql executor: row is missing column %d", expression.ColumnID)
		}
		return value, nil
	case binder.UnaryExpression:
		value, err := evaluate(expression.Arguments[0], row)
		if err != nil || value.IsNull() {
			return value, err
		}
		switch expression.Operator {
		case "+":
			return value, nil
		case "-":
			return negate(value)
		case "NOT":
			boolean, err := value.AsBool()
			return types.BoolValue(!boolean), err
		default:
			return types.Value{}, fmt.Errorf("sql executor: unknown unary operator %q", expression.Operator)
		}
	case binder.BinaryExpression:
		left, err := evaluate(expression.Arguments[0], row)
		if err != nil {
			return types.Value{}, err
		}
		right, err := evaluate(expression.Arguments[1], row)
		if err != nil {
			return types.Value{}, err
		}
		return evaluateBinary(expression, left, right)
	case binder.FunctionExpression:
		if expression.Name == "lower" || expression.Name == "upper" {
			value, err := evaluate(expression.Arguments[0], row)
			if err != nil || value.IsNull() {
				return value, err
			}
			text, err := value.AsText()
			if err != nil {
				return types.Value{}, err
			}
			if expression.Name == "lower" {
				return types.TextValue(strings.ToLower(text))
			}
			return types.TextValue(strings.ToUpper(text))
		}
		return types.Value{}, fmt.Errorf("sql executor: aggregate function %q cannot be evaluated per row", expression.Name)
	default:
		return types.Value{}, fmt.Errorf("sql executor: unknown expression kind %d", expression.Kind)
	}
}

func evaluateBinary(expression *binder.Expression, left, right types.Value) (types.Value, error) {
	operator := expression.Operator
	if operator == "AND" || operator == "OR" {
		return booleanBinary(operator, left, right)
	}
	if left.IsNull() || right.IsNull() {
		return types.NullValue(expression.Type)
	}
	if strings.Contains(" = != <> < <= > >= ", " "+operator+" ") {
		comparison, err := compareValues(left, right)
		if err != nil {
			return types.Value{}, err
		}
		switch operator {
		case "=":
			return types.BoolValue(comparison == 0), nil
		case "!=", "<>":
			return types.BoolValue(comparison != 0), nil
		case "<":
			return types.BoolValue(comparison < 0), nil
		case "<=":
			return types.BoolValue(comparison <= 0), nil
		case ">":
			return types.BoolValue(comparison > 0), nil
		case ">=":
			return types.BoolValue(comparison >= 0), nil
		}
	}
	return arithmetic(expression.Type, operator, left, right)
}

func booleanBinary(operator string, left, right types.Value) (types.Value, error) {
	leftValue, leftKnown, err := nullableBool(left)
	if err != nil {
		return types.Value{}, err
	}
	rightValue, rightKnown, err := nullableBool(right)
	if err != nil {
		return types.Value{}, err
	}
	if operator == "AND" {
		if (leftKnown && !leftValue) || (rightKnown && !rightValue) {
			return types.BoolValue(false), nil
		}
		if leftKnown && rightKnown {
			return types.BoolValue(true), nil
		}
	} else {
		if (leftKnown && leftValue) || (rightKnown && rightValue) {
			return types.BoolValue(true), nil
		}
		if leftKnown && rightKnown {
			return types.BoolValue(false), nil
		}
	}
	return types.NullValue(types.BoolType())
}

func nullableBool(value types.Value) (bool, bool, error) {
	if value.IsNull() {
		return false, false, nil
	}
	boolean, err := value.AsBool()
	return boolean, true, err
}

func compareValues(left, right types.Value) (int, error) {
	if left.IsNull() || right.IsNull() {
		switch {
		case left.IsNull() && right.IsNull():
			return 0, nil
		case left.IsNull():
			return -1, nil
		default:
			return 1, nil
		}
	}
	if isIntegerValue(left) && isIntegerValue(right) {
		leftInteger, _ := left.AsInt64()
		rightInteger, _ := right.AsInt64()
		if leftInteger < rightInteger {
			return -1, nil
		}
		if leftInteger > rightInteger {
			return 1, nil
		}
		return 0, nil
	}
	return types.Compare(left, right)
}

func arithmetic(resultType types.Type, operator string, left, right types.Value) (types.Value, error) {
	if resultType.Kind() == types.DecimalKind {
		if operator != "+" && operator != "-" {
			return types.Value{}, fmt.Errorf("sql executor: DECIMAL operator %s is not implemented", operator)
		}
		leftCoefficient, _ := left.DecimalCoefficient()
		rightCoefficient, _ := right.DecimalCoefficient()
		if operator == "+" {
			leftCoefficient.Add(leftCoefficient, rightCoefficient)
		} else {
			leftCoefficient.Sub(leftCoefficient, rightCoefficient)
		}
		return types.DecimalValueFromCoefficient(resultType, leftCoefficient)
	}
	leftInteger, err := left.AsInt64()
	if err != nil {
		return types.Value{}, err
	}
	rightInteger, err := right.AsInt64()
	if err != nil {
		return types.Value{}, err
	}
	var result *big.Int
	a, b := big.NewInt(leftInteger), big.NewInt(rightInteger)
	switch operator {
	case "+":
		result = new(big.Int).Add(a, b)
	case "-":
		result = new(big.Int).Sub(a, b)
	case "*":
		result = new(big.Int).Mul(a, b)
	case "/":
		if rightInteger == 0 {
			return types.Value{}, fmt.Errorf("sql executor: division by zero")
		}
		result = new(big.Int).Quo(a, b)
	case "%":
		if rightInteger == 0 {
			return types.Value{}, fmt.Errorf("sql executor: division by zero")
		}
		result = new(big.Int).Rem(a, b)
	default:
		return types.Value{}, fmt.Errorf("sql executor: unknown arithmetic operator %q", operator)
	}
	if !result.IsInt64() {
		return types.Value{}, fmt.Errorf("sql executor: integer overflow")
	}
	integer := result.Int64()
	if resultType.Kind() == types.IntKind {
		if integer < math.MinInt32 || integer > math.MaxInt32 {
			return types.Value{}, fmt.Errorf("sql executor: INT overflow")
		}
		return types.IntValue(int32(integer)), nil
	}
	return types.BigIntValue(integer), nil
}

func negate(value types.Value) (types.Value, error) {
	switch value.Type().Kind() {
	case types.IntKind:
		integer, _ := value.AsInt64()
		if integer == math.MinInt32 {
			return types.Value{}, fmt.Errorf("sql executor: INT overflow")
		}
		return types.IntValue(int32(-integer)), nil
	case types.BigIntKind:
		integer, _ := value.AsInt64()
		if integer == math.MinInt64 {
			return types.Value{}, fmt.Errorf("sql executor: BIGINT overflow")
		}
		return types.BigIntValue(-integer), nil
	case types.DecimalKind:
		coefficient, _ := value.DecimalCoefficient()
		coefficient.Neg(coefficient)
		return types.DecimalValueFromCoefficient(value.Type(), coefficient)
	default:
		return types.Value{}, fmt.Errorf("sql executor: cannot negate %s", value.Type())
	}
}

func isIntegerValue(value types.Value) bool {
	return value.Type().Kind() == types.IntKind || value.Type().Kind() == types.BigIntKind
}

func aggregate(expression *binder.Expression, rows []record) (types.Value, error) {
	switch expression.Name {
	case "count":
		if len(expression.Arguments) == 0 {
			return types.BigIntValue(int64(len(rows))), nil
		}
		var count int64
		for _, row := range rows {
			value, err := evaluate(expression.Arguments[0], row.values)
			if err != nil {
				return types.Value{}, err
			}
			if !value.IsNull() {
				count++
			}
		}
		return types.BigIntValue(count), nil
	case "sum":
		var result types.Value
		found := false
		for _, row := range rows {
			value, err := evaluate(expression.Arguments[0], row.values)
			if err != nil {
				return types.Value{}, err
			}
			if value.IsNull() {
				continue
			}
			if !found {
				result, found = value, true
				continue
			}
			result, err = arithmetic(expression.Type, "+", result, value)
			if err != nil {
				return types.Value{}, err
			}
		}
		if !found {
			return types.NullValue(expression.Type)
		}
		return result, nil
	default:
		return types.Value{}, fmt.Errorf("sql executor: unsupported aggregate %q", expression.Name)
	}
}
