package types

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Value is an immutable SQL scalar. A NULL retains its declared SQL type.
type Value struct {
	typeValue Type
	null      bool
	boolean   bool
	integer   int64
	text      string
	decimal   *big.Int
}

func NullValue(typeValue Type) (Value, error) {
	if !typeValue.Valid() {
		return Value{}, fmt.Errorf("types: NULL requires a valid SQL type")
	}
	return Value{typeValue: typeValue, null: true}, nil
}

func BoolValue(value bool) Value {
	return Value{typeValue: BoolType(), boolean: value}
}

func IntValue(value int32) Value {
	return Value{typeValue: IntType(), integer: int64(value)}
}

func BigIntValue(value int64) Value {
	return Value{typeValue: BigIntType(), integer: value}
}

func TextValue(value string) (Value, error) {
	if !utf8.ValidString(value) {
		return Value{}, fmt.Errorf("types: TEXT must contain valid UTF-8")
	}
	return Value{typeValue: TextType(), text: value}, nil
}

func DecimalValue(typeValue Type, raw string) (Value, error) {
	if typeValue.Kind() != DecimalKind || !typeValue.Valid() {
		return Value{}, fmt.Errorf("types: decimal value requires a valid DECIMAL type")
	}
	coefficient, err := parseDecimalCoefficient(raw, typeValue)
	if err != nil {
		return Value{}, err
	}
	return DecimalValueFromCoefficient(typeValue, coefficient)
}

// DecimalValueFromCoefficient constructs a decimal whose integer coefficient is
// interpreted at typeValue.Scale(). The coefficient is copied.
func DecimalValueFromCoefficient(typeValue Type, coefficient *big.Int) (Value, error) {
	if typeValue.Kind() != DecimalKind || !typeValue.Valid() {
		return Value{}, fmt.Errorf("types: decimal coefficient requires a valid DECIMAL type")
	}
	if coefficient == nil {
		return Value{}, fmt.Errorf("types: decimal coefficient cannot be nil")
	}
	if decimalDigits(coefficient) > typeValue.Precision() {
		return Value{}, fmt.Errorf("types: decimal exceeds precision %d", typeValue.Precision())
	}
	return Value{typeValue: typeValue, decimal: new(big.Int).Set(coefficient)}, nil
}

func (value Value) Type() Type   { return value.typeValue }
func (value Value) IsNull() bool { return value.null }

func (value Value) Valid() bool {
	if !value.typeValue.Valid() {
		return false
	}
	if value.null {
		return true
	}
	switch value.typeValue.Kind() {
	case BoolKind:
		return true
	case IntKind:
		return value.integer >= -1<<31 && value.integer <= 1<<31-1
	case BigIntKind:
		return true
	case TextKind:
		return utf8.ValidString(value.text)
	case DecimalKind:
		return value.decimal != nil && decimalDigits(value.decimal) <= value.typeValue.Precision()
	default:
		return false
	}
}

func (value Value) AsBool() (bool, error) {
	if err := value.require(BoolKind); err != nil {
		return false, err
	}
	return value.boolean, nil
}

func (value Value) AsInt64() (int64, error) {
	if value.null {
		return 0, fmt.Errorf("types: NULL has no integer value")
	}
	if value.typeValue.Kind() != IntKind && value.typeValue.Kind() != BigIntKind {
		return 0, fmt.Errorf("types: %s is not an integer", value.typeValue)
	}
	return value.integer, nil
}

func (value Value) AsText() (string, error) {
	if err := value.require(TextKind); err != nil {
		return "", err
	}
	return value.text, nil
}

func (value Value) DecimalCoefficient() (*big.Int, error) {
	if err := value.require(DecimalKind); err != nil {
		return nil, err
	}
	return new(big.Int).Set(value.decimal), nil
}

func (value Value) require(kind Kind) error {
	if value.null {
		return fmt.Errorf("types: NULL has no %s value", kind)
	}
	if value.typeValue.Kind() != kind {
		return fmt.Errorf("types: %s is not %s", value.typeValue, kind)
	}
	return nil
}

func (value Value) String() string {
	if value.null {
		return "NULL"
	}
	switch value.typeValue.Kind() {
	case BoolKind:
		return strconv.FormatBool(value.boolean)
	case IntKind, BigIntKind:
		return strconv.FormatInt(value.integer, 10)
	case TextKind:
		return value.text
	case DecimalKind:
		return formatDecimal(value.decimal, value.typeValue.Scale())
	default:
		return "<invalid>"
	}
}

// Compare provides the total ordering used by indexes. NULL sorts before every
// non-NULL value. Values must have identical declared types.
func Compare(left, right Value) (int, error) {
	if !left.Valid() || !right.Valid() {
		return 0, fmt.Errorf("types: cannot compare invalid values")
	}
	if left.typeValue != right.typeValue {
		return 0, fmt.Errorf("types: cannot compare %s with %s", left.typeValue, right.typeValue)
	}
	if left.null || right.null {
		switch {
		case left.null && right.null:
			return 0, nil
		case left.null:
			return -1, nil
		default:
			return 1, nil
		}
	}
	switch left.typeValue.Kind() {
	case BoolKind:
		if left.boolean == right.boolean {
			return 0, nil
		}
		if !left.boolean {
			return -1, nil
		}
		return 1, nil
	case IntKind, BigIntKind:
		return compareInt64(left.integer, right.integer), nil
	case TextKind:
		return strings.Compare(left.text, right.text), nil
	case DecimalKind:
		return left.decimal.Cmp(right.decimal), nil
	default:
		return 0, fmt.Errorf("types: unsupported comparison type %s", left.typeValue)
	}
}

// Equal reports storage equality, including equality between two typed NULLs.
// SQL expression evaluation will apply three-valued NULL semantics separately.
func Equal(left, right Value) bool {
	comparison, err := Compare(left, right)
	return err == nil && comparison == 0
}

func parseDecimalCoefficient(raw string, typeValue Type) (*big.Int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, fmt.Errorf("types: decimal value cannot be empty")
	}
	negative := false
	if value[0] == '+' || value[0] == '-' {
		negative = value[0] == '-'
		value = value[1:]
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" {
		return nil, fmt.Errorf("types: invalid decimal %q", raw)
	}
	integerPart := parts[0]
	fractionalPart := ""
	if len(parts) == 2 {
		fractionalPart = parts[1]
	}
	if !allDecimalDigits(integerPart) || !allDecimalDigits(fractionalPart) {
		return nil, fmt.Errorf("types: invalid decimal %q", raw)
	}
	if len(fractionalPart) > typeValue.Scale() {
		return nil, fmt.Errorf("types: decimal %q exceeds scale %d", raw, typeValue.Scale())
	}
	fractionalPart += strings.Repeat("0", typeValue.Scale()-len(fractionalPart))
	digits := strings.TrimLeft(integerPart+fractionalPart, "0")
	if digits == "" {
		digits = "0"
	}
	coefficient, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, fmt.Errorf("types: invalid decimal %q", raw)
	}
	if negative && coefficient.Sign() != 0 {
		coefficient.Neg(coefficient)
	}
	if decimalDigits(coefficient) > typeValue.Precision() {
		return nil, fmt.Errorf("types: decimal %q exceeds precision %d", raw, typeValue.Precision())
	}
	return coefficient, nil
}

func formatDecimal(coefficient *big.Int, scale int) string {
	negative := coefficient.Sign() < 0
	digits := new(big.Int).Abs(new(big.Int).Set(coefficient)).String()
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		split := len(digits) - scale
		digits = digits[:split] + "." + digits[split:]
	}
	if negative {
		return "-" + digits
	}
	return digits
}

func decimalDigits(value *big.Int) int {
	if value.Sign() == 0 {
		return 1
	}
	return len(new(big.Int).Abs(new(big.Int).Set(value)).String())
}

func allDecimalDigits(value string) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func compareInt64(left, right int64) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}
