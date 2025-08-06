package db

import "pebbledb/pagemanager"

type FieldType string

const (
	TypeInt    FieldType = "INT"
	TypeString FieldType = "STRING"
)

type Column struct {
	Name string
	Type FieldType
}

type TuplePointer struct {
	PageID int
	Offset int
}
type Row struct {
	Value        map[string]interface{}
	TuplePointer *TuplePointer
}

type Table struct {
	Name        string
	Columns     []Column
	PageNo      []int
	PageManager *pagemanager.PageManager
}
