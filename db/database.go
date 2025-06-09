package db

import "fmt"

type Database struct {
	Tables map[string]*Table
}

func NewDatabase() *Database {
	return &Database{
		Tables: make(map[string]*Table),
	}
}

func (db *Database) CreateTable(name string, columns []Column) error {
	if _, exists := db.Tables[name]; exists {
		return fmt.Errorf("table %s already exists", name)
	}
	table := NewTable(name, columns)
	db.Tables[name] = table

	return nil
}

func (db *Database) GetTable(name string) (*Table, error) {
	if table, exists := db.Tables[name]; exists {
		return table, nil
	} else {
		return nil, fmt.Errorf("table %s does not exist", name)
	}
}

func (db *Database) GetAllTables() []*Table {
	var tables []*Table
	for _, table := range db.Tables {
		tables = append(tables, table)
	}
	return tables
}

func (db *Database) DropTable(name string) error {
	if _, exists := db.Tables[name]; exists {
		delete(db.Tables, name)
		return nil
	}
	return fmt.Errorf("table %s does not exist", name)
}

func (db *Database) InsertValue(tablename string, values []string) error {
	table, exists := db.Tables[tablename]
	if exists {
		table.Insert(values)
		return nil
	}
	return fmt.Errorf("table %s does not exist", tablename)
}

func (db *Database) SelectAll(tableName string) ([]Row, error) {
	table, exists := db.Tables[tableName]
	if !exists {
		return nil, fmt.Errorf("table %s does not exist", tableName)
	}
	return table.Rows, nil
}
