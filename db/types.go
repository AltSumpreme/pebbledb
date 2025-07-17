package db

type FieldType string

const (
	TypeInt    FieldType = "INT"
	TypeString FieldType = "STRING"
)

type Column struct {
	Name string
	Type FieldType
}

type Row map[string]interface{}

type Table struct {
	Name    string
	Columns []Column
	Rows    []Row
}
