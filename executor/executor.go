package executor

import (
	"fmt"
	"pebbledb/db"
	"pebbledb/parser"
	"pebbledb/storage"
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
		storage.SaveToDisk(database)
		return &ExecutionResult{Message: "Table created successfully"}
	case parser.CommandTypeInsert:
		if err = database.InsertValue(command.Tablename, command.Values); err != nil {
			return &ExecutionResult{Error: err}
		}
		storage.SaveToDisk(database)
		return &ExecutionResult{Message: "Value inserted successfully"}
	case parser.CommandTypeSelect:

		latestDB, err := storage.LoadFromDisk()
		if err != nil {
			return &ExecutionResult{Error: err}
		}
		*database = *latestDB
		if command.AllColumns {
			rows, err = database.SelectAll(command.Tablename)
			if err != nil {
				fmt.Println("Error selecting all columns:", err)
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
		fmt.Printf("Returning %d rows from ExecuteCommand\n", len(rows))

		return &ExecutionResult{Message: "Query executed successfully", Rows: rows}

	case parser.CommandTypeDrop:
		if err = database.DropTable(command.Tablename); err != nil {
			return &ExecutionResult{Error: err}
		}
		storage.SaveToDisk(database)
		return &ExecutionResult{Message: "Table dropped successfully"}
	}
	return &ExecutionResult{Error: fmt.Errorf("unknown command type: %s", command.Type)}
}
