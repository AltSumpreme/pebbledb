// Package types defines PebbleDB's SQL type system and immutable typed values.
package types

import (
	"fmt"
	"strconv"
	"strings"
)

const MaxDecimalPrecision = 38

// Kind is the stable identifier written into PebbleDB's persistent formats.
type Kind uint8

const (
	UnknownKind Kind = iota
	BoolKind
	IntKind
	BigIntKind
	TextKind
	DecimalKind
)

func (kind Kind) String() string {
	switch kind {
	case BoolKind:
		return "BOOL"
	case IntKind:
		return "INT"
	case BigIntKind:
		return "BIGINT"
	case TextKind:
		return "TEXT"
	case DecimalKind:
		return "DECIMAL"
	default:
		return "UNKNOWN"
	}
}

// Type describes a SQL scalar type. Type values are immutable and comparable.
type Type struct {
	kind      Kind
	precision uint8
	scale     uint8
}

func BoolType() Type   { return Type{kind: BoolKind} }
func IntType() Type    { return Type{kind: IntKind} }
func BigIntType() Type { return Type{kind: BigIntKind} }
func TextType() Type   { return Type{kind: TextKind} }

func DecimalType(precision, scale int) (Type, error) {
	return NewType(DecimalKind, precision, scale)
}

// NewType validates and constructs a type from its persistent descriptor.
func NewType(kind Kind, precision, scale int) (Type, error) {
	typeValue := Type{kind: kind}
	switch kind {
	case BoolKind, IntKind, BigIntKind, TextKind:
		if precision != 0 || scale != 0 {
			return Type{}, fmt.Errorf("types: %s does not accept precision or scale", kind)
		}
	case DecimalKind:
		if precision < 1 || precision > MaxDecimalPrecision {
			return Type{}, fmt.Errorf("types: decimal precision must be between 1 and %d", MaxDecimalPrecision)
		}
		if scale < 0 || scale > precision {
			return Type{}, fmt.Errorf("types: decimal scale must be between 0 and precision")
		}
		typeValue.precision = uint8(precision)
		typeValue.scale = uint8(scale)
	default:
		return Type{}, fmt.Errorf("types: unknown type kind %d", kind)
	}
	return typeValue, nil
}

func (typeValue Type) Kind() Kind     { return typeValue.kind }
func (typeValue Type) Precision() int { return int(typeValue.precision) }
func (typeValue Type) Scale() int     { return int(typeValue.scale) }

func (typeValue Type) Valid() bool {
	_, err := NewType(typeValue.kind, int(typeValue.precision), int(typeValue.scale))
	return err == nil
}

func (typeValue Type) String() string {
	if typeValue.kind == DecimalKind {
		return fmt.Sprintf("DECIMAL(%d,%d)", typeValue.precision, typeValue.scale)
	}
	return typeValue.kind.String()
}

// ParseType accepts the initial PostgreSQL-compatible names and aliases.
func ParseType(raw string) (Type, error) {
	normalized := strings.ToUpper(strings.TrimSpace(raw))
	switch normalized {
	case "BOOL", "BOOLEAN":
		return BoolType(), nil
	case "INT", "INTEGER", "INT4":
		return IntType(), nil
	case "BIGINT", "INT8":
		return BigIntType(), nil
	case "TEXT", "STRING":
		return TextType(), nil
	}

	open := strings.IndexByte(normalized, '(')
	if open < 0 || !strings.HasSuffix(normalized, ")") {
		return Type{}, fmt.Errorf("types: unsupported SQL type %q", raw)
	}
	name := strings.TrimSpace(normalized[:open])
	if name != "DECIMAL" && name != "NUMERIC" {
		return Type{}, fmt.Errorf("types: unsupported SQL type %q", raw)
	}
	parameters := strings.Split(normalized[open+1:len(normalized)-1], ",")
	if len(parameters) != 2 {
		return Type{}, fmt.Errorf("types: decimal requires precision and scale")
	}
	precision, err := strconv.Atoi(strings.TrimSpace(parameters[0]))
	if err != nil {
		return Type{}, fmt.Errorf("types: invalid decimal precision: %w", err)
	}
	scale, err := strconv.Atoi(strings.TrimSpace(parameters[1]))
	if err != nil {
		return Type{}, fmt.Errorf("types: invalid decimal scale: %w", err)
	}
	return DecimalType(precision, scale)
}

func (typeValue Type) MarshalText() ([]byte, error) {
	if !typeValue.Valid() {
		return nil, fmt.Errorf("types: cannot marshal invalid type")
	}
	return []byte(typeValue.String()), nil
}

func (typeValue *Type) UnmarshalText(text []byte) error {
	parsed, err := ParseType(string(text))
	if err != nil {
		return err
	}
	*typeValue = parsed
	return nil
}
