package pebbledb

import (
	"os"
	"pebbledb/db"
	"pebbledb/pager"
	"pebbledb/storage"
)

type Engine struct {
	DB    *db.Database
	Pager *pager.Pager
}

func NewEngine() (*Engine, error) {

	var database *db.Database
	if _, err := os.Stat(storage.Path); err == nil {
		database, err = storage.LoadFromDisk()
		if err != nil {
			return nil, err
		}
	} else if os.IsNotExist(err) {
		database = db.NewDatabase()
	} else {
		return nil, err
	}
	pagerFile, err := os.OpenFile(storage.Path, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		return nil, err
	}
	pgr := pager.PagerInit(pagerFile)
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
	return engine.Pager.Close()
}
