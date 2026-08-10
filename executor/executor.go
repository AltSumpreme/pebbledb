package executor

import (
	"fmt"
	"pebbledb/db"
	"pebbledb/parser"
)

type ExecutionResult struct {
	Message string
	Rows    []db.Row
	Error   error
}

func ExecuteCommand(command *parser.Command, database *db.Database) *ExecutionResult {
	var rows []db.Row
	var err error
	switch command.Type {
	case parser.CommandTypeCreate:
		if err = database.CreateTable(command.Tablename, command.Columns); err != nil {
			return &ExecutionResult{Error: err}
		}
		return &ExecutionResult{Message: "Table created successfully"}
	case parser.CommandTypeInsert:
		if err = database.InsertColumns(command.Tablename, command.Columns, command.Values); err != nil {
			return &ExecutionResult{Error: err}
		}
		return &ExecutionResult{Message: "Value inserted successfully"}
	case parser.CommandTypeSelect:

		if command.AllColumns {
			rows, err = database.SelectAll(command.Tablename)
			if err != nil {
				return &ExecutionResult{Error: err}
			}
			if len(rows) == 0 {
				return &ExecutionResult{Message: "No rows found"}
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

		return &ExecutionResult{Message: "Query executed successfully", Rows: rows}

	case parser.CommandTypeDrop:
		if err = database.DropTable(command.Tablename); err != nil {
			return &ExecutionResult{Error: err}
		}
		return &ExecutionResult{Message: "Table dropped successfully"}
	}
	return &ExecutionResult{Error: fmt.Errorf("unknown command type: %s", command.Type)}
}
