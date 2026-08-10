// Package engine composes the SQL frontend, planner, executor, catalog, MVCC,
// and LSM storage into a single-node SQL database.
package engine

import (
	"fmt"
	"sync"

	"pebbledb/catalog"
	"pebbledb/sql/ast"
	"pebbledb/sql/binder"
	"pebbledb/sql/executor"
	"pebbledb/sql/parser"
	"pebbledb/sql/plan"
	"pebbledb/storage/indexstore"
	"pebbledb/storage/kv"
	"pebbledb/storage/lsm"
	"pebbledb/storage/mvcc"
	"pebbledb/storage/rowstore"
)

type Engine struct {
	mu         sync.Mutex
	store      *lsm.Store
	manager    *mvcc.Manager
	catalog    *catalog.Catalog
	databaseID catalog.DescriptorID
	schemaID   catalog.DescriptorID
	active     *mvcc.Transaction
	failed     bool
	closed     bool
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
	manager, err := mvcc.NewManager(store)
	if err != nil {
		return fail(err)
	}
	catalogValue, err := catalog.New(manager.Store())
	if err != nil {
		return fail(err)
	}
	database, schema, err := catalogValue.EnsureDefaults()
	if err != nil {
		return fail(err)
	}
	return &Engine{
		store:      store,
		manager:    manager,
		catalog:    catalogValue,
		databaseID: database.ID,
		schemaID:   schema.ID,
	}, nil
}

// Execute parses and executes semicolon-separated SQL statements in order.
// Statements use serializable autocommit unless enclosed by BEGIN and COMMIT.
func (engine *Engine) Execute(sql string) ([]executor.Result, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closed {
		return nil, fmt.Errorf("sql engine: database is closed")
	}
	statements, err := parser.ParseStatements(sql)
	if err != nil {
		if engine.active != nil {
			engine.failed = true
		}
		return nil, err
	}
	results := make([]executor.Result, 0, len(statements))
	for index, statement := range statements {
		if result, handled, err := engine.transactionControl(statement); handled {
			if err != nil {
				return results, fmt.Errorf("statement %d: %w", index+1, err)
			}
			results = append(results, result)
			continue
		}
		if engine.failed {
			return results, fmt.Errorf("statement %d: current transaction is aborted; ROLLBACK is required", index+1)
		}
		transaction := engine.active
		autocommit := transaction == nil
		if autocommit {
			transaction = engine.manager.Begin()
		}
		result, err := engine.executeStatement(transaction, statement)
		if err == nil && autocommit {
			_, err = transaction.Commit()
		}
		if err != nil {
			if autocommit {
				transaction.Rollback()
			} else {
				engine.failed = true
			}
			return results, fmt.Errorf("statement %d: %w", index+1, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (engine *Engine) transactionControl(statement ast.Statement) (executor.Result, bool, error) {
	switch statement.(type) {
	case ast.Begin:
		if engine.active != nil {
			return executor.Result{}, true, fmt.Errorf("transaction is already active")
		}
		engine.active = engine.manager.Begin()
		engine.failed = false
		return executor.Result{Message: "BEGIN"}, true, nil
	case ast.Commit:
		if engine.active == nil {
			return executor.Result{}, true, fmt.Errorf("no transaction is active")
		}
		if engine.failed {
			engine.active.Rollback()
			engine.active, engine.failed = nil, false
			return executor.Result{}, true, fmt.Errorf("transaction was aborted and rolled back")
		}
		_, err := engine.active.Commit()
		engine.active, engine.failed = nil, false
		if err != nil {
			return executor.Result{}, true, err
		}
		return executor.Result{Message: "COMMIT"}, true, nil
	case ast.Rollback:
		if engine.active == nil {
			return executor.Result{}, true, fmt.Errorf("no transaction is active")
		}
		engine.active.Rollback()
		engine.active, engine.failed = nil, false
		return executor.Result{Message: "ROLLBACK"}, true, nil
	default:
		return executor.Result{}, false, nil
	}
}

func (engine *Engine) executeStatement(store kv.Store, statement ast.Statement) (executor.Result, error) {
	catalogValue, binderValue, executorValue, indexes, err := engine.components(store)
	_ = catalogValue
	if err != nil {
		return executor.Result{}, err
	}
	bound, err := binderValue.Bind(statement)
	if err != nil {
		return executor.Result{}, fmt.Errorf("bind: %w", err)
	}
	logical, err := plan.Build(bound)
	if err != nil {
		return executor.Result{}, fmt.Errorf("plan: %w", err)
	}
	physical, err := plan.Optimize(logical, indexes)
	if err != nil {
		return executor.Result{}, fmt.Errorf("physical plan: %w", err)
	}
	result, err := executorValue.Execute(physical)
	if err != nil {
		return executor.Result{}, fmt.Errorf("execute: %w", err)
	}
	return result, nil
}

func (engine *Engine) components(store kv.Store) (*catalog.Catalog, *binder.Binder, *executor.Executor, *indexstore.Store, error) {
	catalogValue, err := catalog.New(store)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	binderValue, err := binder.New(catalogValue, binder.Context{DatabaseID: engine.databaseID, SchemaID: engine.schemaID})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	rows, err := rowstore.New(store)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	indexes, err := indexstore.New(store)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	executorValue, err := executor.New(catalogValue, rows, indexes)
	return catalogValue, binderValue, executorValue, indexes, err
}

func (engine *Engine) Explain(sql string) (string, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closed {
		return "", fmt.Errorf("sql engine: database is closed")
	}
	statement, err := parser.Parse(sql)
	if err != nil {
		return "", err
	}
	if engine.failed {
		return "", fmt.Errorf("current transaction is aborted; ROLLBACK is required")
	}
	transaction := engine.active
	temporary := transaction == nil
	if temporary {
		transaction = engine.manager.Begin()
		defer transaction.Rollback()
	}
	_, binderValue, _, indexes, err := engine.components(transaction)
	if err != nil {
		return "", err
	}
	bound, err := binderValue.Bind(statement)
	if err != nil {
		return "", err
	}
	logical, err := plan.Build(bound)
	if err != nil {
		return "", err
	}
	physical, err := plan.Optimize(logical, indexes)
	if err != nil {
		return "", err
	}
	return plan.Explain(physical), nil
}

// Catalog exposes an autocommit administrative catalog view.
func (engine *Engine) Catalog() *catalog.Catalog { return engine.catalog }

func (engine *Engine) Close() error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closed {
		return nil
	}
	if engine.active != nil {
		engine.active.Rollback()
		engine.active = nil
	}
	engine.closed = true
	return engine.store.Close()
}
