package db

type FieldType string

const (
	TypeInt    FieldType = "INT"
	TypeString FieldType = "STRING"
)

type Column struct {
	Name string    `json:"name"`
	Type FieldType `json:"type"`
}

type Row map[string]interface{}

type Table struct {
	Name    string   `json:"name"`
	Columns []Column `json:"columns"`
	Rows    []Row    `json:"rows"`
}
