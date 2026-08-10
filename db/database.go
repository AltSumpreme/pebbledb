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
	name = NormalizeIdentifier(name)
	if _, exists := db.Tables[name]; exists {
		return fmt.Errorf("table %s already exists", name)
	}
	seenColumns := make(map[string]struct{}, len(columns))
	for i := range columns {
		columns[i].Name = NormalizeIdentifier(columns[i].Name)
		if columns[i].Name == "" {
			return fmt.Errorf("column name cannot be empty")
		}
		if _, exists := seenColumns[columns[i].Name]; exists {
			return fmt.Errorf("column %s already exists", columns[i].Name)
		}
		seenColumns[columns[i].Name] = struct{}{}
	}
	table := NewTable(name, columns)
	db.Tables[name] = table

	return nil
}

func (db *Database) GetTable(name string) (*Table, error) {
	name = NormalizeIdentifier(name)
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
	name = NormalizeIdentifier(name)
	if _, exists := db.Tables[name]; exists {
		delete(db.Tables, name)
		return nil
	}
	return fmt.Errorf("table %s does not exist", name)
}

func (db *Database) InsertValue(tablename string, values []string) error {
	tablename = NormalizeIdentifier(tablename)
	table, exists := db.Tables[tablename]
	if exists {
		return table.Insert(values)
	}
	return fmt.Errorf("table %s does not exist", tablename)
}

func (db *Database) InsertColumns(tablename string, columns []Column, values []string) error {
	tablename = NormalizeIdentifier(tablename)
	table, exists := db.Tables[tablename]
	if !exists {
		return fmt.Errorf("table %s does not exist", tablename)
	}
	return table.InsertColumns(columns, values)
}

func (db *Database) SelectAll(tableName string) ([]Row, error) {
	tableName = NormalizeIdentifier(tableName)
	table, exists := db.Tables[tableName]
	if !exists {
		return nil, fmt.Errorf("table %s does not exist", tableName)
	}
	return table.Rows, nil
}

func (db *Database) SelectColumns(tableName string, columns []Column) ([]Row, error) {
	tableName = NormalizeIdentifier(tableName)
	table, exists := db.Tables[tableName]
	if !exists {
		return nil, fmt.Errorf("table %s does not exist", tableName)
	}
	var result []Row
	for _, row := range table.Rows {
		selectedRow := Row{}
		for _, col := range columns {
			name := NormalizeIdentifier(col.Name)
			if value, ok := row[name]; ok {
				selectedRow[name] = value
			} else {
				return nil, fmt.Errorf("column %s does not exist in table %s", name, tableName)
			}
		}
		result = append(result, selectedRow)
	}
	return result, nil
}
