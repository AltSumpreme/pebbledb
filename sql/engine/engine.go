// Package engine composes the new SQL frontend, planner, executor, catalog, and
// LSM-backed row store into a single-node SQL engine.
package engine

import (
	"fmt"

	"pebbledb/catalog"
	"pebbledb/sql/binder"
	"pebbledb/sql/executor"
	"pebbledb/sql/parser"
	"pebbledb/sql/plan"
	"pebbledb/storage/indexstore"
	"pebbledb/storage/lsm"
	"pebbledb/storage/rowstore"
)

type Engine struct {
	store    *lsm.Store
	catalog  *catalog.Catalog
	binder   *binder.Binder
	executor *executor.Executor
	indexes  *indexstore.Store
}

func Open(directory string) (*Engine, error) {
	store, err := lsm.Open(directory)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Engine, error) {
		_ = store.Close()
		return nil, err
	}
	catalogValue, err := catalog.New(store)
	if err != nil {
		return fail(err)
	}
	database, schema, err := catalogValue.EnsureDefaults()
	if err != nil {
		return fail(err)
	}
	binderValue, err := binder.New(catalogValue, binder.Context{DatabaseID: database.ID, SchemaID: schema.ID})
	if err != nil {
		return fail(err)
	}
	rows, err := rowstore.New(store)
	if err != nil {
		return fail(err)
	}
	indexes, err := indexstore.New(store)
	if err != nil {
		return fail(err)
	}
	executorValue, err := executor.New(catalogValue, rows, indexes)
	if err != nil {
		return fail(err)
	}
	return &Engine{store: store, catalog: catalogValue, binder: binderValue, executor: executorValue, indexes: indexes}, nil
}

// Execute parses and executes one or more semicolon-separated SQL statements in
// order. Earlier successful statements are not rolled back if a later statement
// fails; explicit atomic transactions arrive in Milestone 7.
func (engine *Engine) Execute(sql string) ([]executor.Result, error) {
	statements, err := parser.ParseStatements(sql)
	if err != nil {
		return nil, err
	}
	results := make([]executor.Result, 0, len(statements))
	for index, statement := range statements {
		bound, err := engine.binder.Bind(statement)
		if err != nil {
			return results, fmt.Errorf("statement %d bind: %w", index+1, err)
		}
		logical, err := plan.Build(bound)
		if err != nil {
			return results, fmt.Errorf("statement %d plan: %w", index+1, err)
		}
		physical, err := plan.Optimize(logical, engine.indexes)
		if err != nil {
			return results, fmt.Errorf("statement %d physical plan: %w", index+1, err)
		}
		result, err := engine.executor.Execute(physical)
		if err != nil {
			return results, fmt.Errorf("statement %d execute: %w", index+1, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (engine *Engine) Explain(sql string) (string, error) {
	statement, err := parser.Parse(sql)
	if err != nil {
		return "", err
	}
	bound, err := engine.binder.Bind(statement)
	if err != nil {
		return "", err
	}
	logical, err := plan.Build(bound)
	if err != nil {
		return "", err
	}
	physical, err := plan.Optimize(logical, engine.indexes)
	if err != nil {
		return "", err
	}
	return plan.Explain(physical), nil
}

func (engine *Engine) Catalog() *catalog.Catalog { return engine.catalog }

func (engine *Engine) Close() error { return engine.store.Close() }
