package db

import (
	"fmt"
	"strconv"
	"strings"
)

func NewTable(name string, columns []Column) *Table {
	normalizedColumns := make([]Column, len(columns))
	for i, col := range columns {
		normalizedColumns[i] = Column{
			Name: NormalizeIdentifier(col.Name),
			Type: col.Type,
		}
	}
	return &Table{
		Name:    NormalizeIdentifier(name),
		Columns: normalizedColumns,
		Rows:    make([]Row, 0),
	}
}

func (t *Table) Insert(values []string) error {
	if len(values) != len(t.Columns) {
		return fmt.Errorf("number of values does not match number of columns")
	}
	row := make(Row)

	for i, col := range t.Columns {
		typedValue, err := parseValue(col, values[i])
		if err != nil {
			return err
		}
		row[col.Name] = typedValue
	}
	t.Rows = append(t.Rows, row)
	return nil
}

func (t *Table) InsertColumns(columns []Column, values []string) error {
	if len(columns) != len(values) {
		return fmt.Errorf("number of columns and values do not match")
	}
	row := make(Row)
	for _, col := range t.Columns {
		row[col.Name] = zeroValue(col.Type)
	}

	columnByName := make(map[string]Column, len(t.Columns))
	for _, col := range t.Columns {
		columnByName[col.Name] = col
	}

	seen := make(map[string]struct{}, len(columns))
	for i, requested := range columns {
		name := NormalizeIdentifier(requested.Name)
		if _, exists := seen[name]; exists {
			return fmt.Errorf("column %s specified more than once", name)
		}
		seen[name] = struct{}{}

		tableColumn, ok := columnByName[name]
		if !ok {
			return fmt.Errorf("column %s does not exist in table %s", name, t.Name)
		}
		typedValue, err := parseValue(tableColumn, values[i])
		if err != nil {
			return err
		}
		row[name] = typedValue
	}

	t.Rows = append(t.Rows, row)
	return nil
}

func NormalizeIdentifier(identifier string) string {
	return strings.ToLower(strings.TrimSpace(identifier))
}

func parseValue(col Column, raw string) (interface{}, error) {
	value := strings.TrimSpace(raw)
	switch col.Type {
	case TypeInt:
		intValue, err := strconv.Atoi(value)
		if err != nil {
			return nil, fmt.Errorf("invalid value for column %s: %s", col.Name, raw)
		}
		return intValue, nil
	case TypeString:
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported column type %s for column %s", col.Type, col.Name)
	}
}

func zeroValue(fieldType FieldType) interface{} {
	switch fieldType {
	case TypeInt:
		return 0
	case TypeString:
		return ""
	default:
		return nil
	}
}
