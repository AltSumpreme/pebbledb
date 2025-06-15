package executor

import (
	"pebbledb/db"
	"pebbledb/parser"
)

type ExecutionResult struct {
	Message string
	Rows    []db.Row
	Error   error
}

func ExecuteCommand(command parser.Command, database *db.Database) *ExecutionResult {
	var rows []db.Row
	var err error
	switch command.Type {
	case parser.CommandTypeCreate:
		if err = database.CreateTable(command.Tablename, command.Columns); err != nil {
			return &ExecutionResult{Error: err}
		}
		return &ExecutionResult{Message: "Table created successfully"}
	case parser.CommandTypeInsert:
		if err = database.InsertValue(command.Tablename, command.Values); err != nil {
			return &ExecutionResult{Error: err}
		}
		return &ExecutionResult{Message: "Value inserted successfully"}
	case parser.CommandTypeSelect:
		var rows []db.Row
		var err error
		if len(command.Columns) == 1 && command.Columns[0].Name == "*" {
			rows, err = database.SelectAll(command.Tablename)
			if err != nil {
				return &ExecutionResult{Error: err}
			}
		} else {
			rows, err = database.SelectColumns(command.Tablename, command.Columns)
			if err != nil {
				return &ExecutionResult{Error: err}
			}
			if len(rows) == 0 {
				return &ExecutionResult{Message: "No rows found"}
			}

		}
	}
	return &ExecutionResult{Rows: rows}
}
