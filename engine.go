package pebbledb

import (
	"os"
	"pebbledb/db"
	"pebbledb/executor"
	"pebbledb/pager"
	"pebbledb/parser"
	"pebbledb/storage"
)

type Engine struct {
	DB      *db.Database
	Pager   *pager.Page
	DataDir string
}

func NewEngine(dataDir ...string) (*Engine, error) {
	dir := storage.DefaultDataDir
	if len(dataDir) > 0 && dataDir[0] != "" {
		dir = dataDir[0]
	}

	var database *db.Database
	if _, err := os.Stat(dir); err == nil {
		database, err = storage.LoadFromDisk(dir)
		if err != nil {
			return nil, err
		}
	} else if os.IsNotExist(err) {
		database = db.NewDatabase()
	} else {
		return nil, err
	}
	err := os.MkdirAll(dir, 0775)
	if err != nil {
		return nil, err
	}
	pgr := pager.NewPage()
	if pgr == nil {
		return nil, err
	}

	engine := &Engine{
		DB:      database,
		Pager:   pgr,
		DataDir: dir,
	}

	return engine, nil
}

func (engine *Engine) Execute(command *parser.Command) *executor.ExecutionResult {
	result := executor.ExecuteCommand(command, engine.DB)
	if result.Error != nil || !isMutating(command.Type) {
		return result
	}
	if err := storage.SaveToDisk(engine.DB, engine.DataDir); err != nil {
		return &executor.ExecutionResult{Error: err}
	}
	return result
}

func (engine *Engine) Close() error {
	if err := storage.SaveToDisk(engine.DB, engine.DataDir); err != nil {
		return err
	}

	return nil
}

func isMutating(commandType parser.CommandType) bool {
	switch commandType {
	case parser.CommandTypeCreate, parser.CommandTypeInsert, parser.CommandTypeDrop, parser.CommandTypeDelete:
		return true
	default:
		return false
	}
}
