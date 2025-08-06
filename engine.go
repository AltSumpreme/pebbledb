package pebbledb

import (
	"os"
	"pebbledb/db"
	"pebbledb/pagemanager"
	"pebbledb/pager"
	"pebbledb/storage"
)

type Engine struct {
	DB    *db.Database
	Pager *pager.Page
}

func NewEngine() (*Engine, error) {

	var database *db.Database
	pm := pagemanager.NewPageManager()
	if _, err := os.Stat(storage.DBDir); err == nil {
		database, err = storage.LoadFromDisk()
		if err != nil {
			return nil, err
		}
	} else if os.IsNotExist(err) {
		database = db.NewDatabase()
	} else {
		return nil, err
	}
	err := os.MkdirAll(storage.DBDir, 0775)
	if err != nil {
		return nil, err
	}
	pgr := pager.PageInit()
	if pgr == nil {
		return nil, err
	}

	engine := &Engine{
		DB:    database,
		Pager: pgr,
	}

	return engine, nil
}

func (engine *Engine) Close() error {
	if err := storage.SaveToDisk(engine.DB); err != nil {
		return err
	}

	return nil
}
