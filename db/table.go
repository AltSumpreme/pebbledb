package db

import "fmt"

func NewTable(name string, columns []Column) *Table {
	return &Table{
		Name:    name,
		Columns: columns,
		Rows:    make([]Row, 0),
	}
}

func (t *Table) Insert(values []string) error {
	if len(values) != len(t.Columns) {
		return fmt.Errorf("number of values does not match number of columns")
	}
	row := make(Row)

	for i, col := range t.Columns {
		switch col.Type {
		case TypeInt:
			var intValue int
			_, err := fmt.Sscanf(values[i], "%d", &intValue)
			if err != nil {
				return fmt.Errorf("invalid value for column %s: %s", col.Name, values[i])
			}
			row[col.Name] = intValue

		case TypeString:
			var strValue string
			_, err := fmt.Sscanf(values[i], "%s", &strValue)
			if err != nil {
				return fmt.Errorf("invalid value for column %s: %s", col.Name, values[i])
			}
			row[col.Name] = strValue
		default:
			return fmt.Errorf("unsupported column type %s for column %s", col.Type, col.Name)
		}
	}
	t.Rows = append(t.Rows, row)
	return nil
}
