// Package engine composes the SQL frontend, planner, executor, catalog, MVCC,
// and LSM storage into a single-node SQL database.
package engine

import (
	"context"
	"fmt"
	"sync"

	"pebbledb/catalog"
	"pebbledb/distributed/query"
	"pebbledb/distributed/raft"
	"pebbledb/distributed/ranges"
	"pebbledb/distributed/upgrade"
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
	raftNode   *raft.Node
	raftGroup  *raft.Group
	ranges     *ranges.Router
	upgrade    *upgrade.Coordinator
	nodeID     string
	manager    *mvcc.Manager
	catalog    *catalog.Catalog
	databaseID catalog.DescriptorID
	schemaID   catalog.DescriptorID
	active     *mvcc.Transaction
	failed     bool
	closed     bool
}

type Diagnostics struct {
	Storage   lsm.Stats
	Consensus raft.Status
	Ranges    []ranges.Descriptor
	Upgrade   upgrade.Status
}

func Open(directory string) (*Engine, error) {
	return OpenWithOptions(directory, Options{})
}

// Options identifies this binary during rolling cluster-version changes.
// Zero values select the current binary's full compatibility interval.
type Options struct {
	NodeID           string
	MinBinaryVersion upgrade.Version
	MaxBinaryVersion upgrade.Version
}

func (options Options) normalized() Options {
	if options.NodeID == "" {
		options.NodeID = "node-1"
	}
	if options.MinBinaryVersion == 0 {
		options.MinBinaryVersion = upgrade.MinimumVersion
	}
	if options.MaxBinaryVersion == 0 {
		options.MaxBinaryVersion = upgrade.CurrentVersion
	}
	return options
}

// OpenWithOptions opens an engine and registers its supported cluster-version
// interval. An old binary is refused after a newer version is activated.
func OpenWithOptions(directory string, options Options) (*Engine, error) {
	options = options.normalized()
	if options.MinBinaryVersion < upgrade.MinimumVersion || options.MaxBinaryVersion > upgrade.CurrentVersion || options.MinBinaryVersion > options.MaxBinaryVersion {
		return nil, fmt.Errorf("sql engine: invalid binary compatibility interval %d-%d", options.MinBinaryVersion, options.MaxBinaryVersion)
	}
	store, err := lsm.Open(directory)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Engine, error) {
		_ = store.Close()
		return nil, err
	}
	spanStart, spanEnd := mvcc.PhysicalKeySpan()
	machine, err := raft.NewKVStateMachine(store, spanStart, spanEnd)
	if err != nil {
		return fail(err)
	}
	raftNode, err := raft.OpenNode(1, 1, store, machine, []uint64{1})
	if err != nil {
		return fail(err)
	}
	raftGroup, err := raft.NewGroup(1, []*raft.Node{raftNode}, []uint64{1})
	if err != nil {
		return fail(err)
	}
	if err := raftGroup.Elect(1); err != nil {
		return fail(err)
	}
	replicated, err := raft.NewReplicatedStore(raftGroup, store)
	if err != nil {
		return fail(err)
	}
	rangeRouter, err := ranges.Open(store, ranges.Replica{ID: 1, Store: replicated})
	if err != nil {
		return fail(err)
	}
	manager, err := mvcc.NewManager(rangeRouter)
	if err != nil {
		return fail(err)
	}
	bootstrapVersion := upgrade.BaselineVersion
	existing, err := manager.Store().Scan(nil, nil)
	if err != nil {
		return fail(err)
	}
	if len(existing) == 0 {
		bootstrapVersion = options.MaxBinaryVersion
	}
	upgradeCoordinator, err := upgrade.Open(manager.Store(), bootstrapVersion)
	if err != nil {
		return fail(err)
	}
	if err := upgradeCoordinator.Register(upgrade.Binary{
		NodeID: options.NodeID, Min: options.MinBinaryVersion, Max: options.MaxBinaryVersion,
	}); err != nil {
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
		raftNode:   raftNode,
		raftGroup:  raftGroup,
		ranges:     rangeRouter,
		upgrade:    upgradeCoordinator,
		nodeID:     options.NodeID,
		manager:    manager,
		catalog:    catalogValue,
		databaseID: database.ID,
		schemaID:   schema.ID,
	}, nil
}

// Execute parses and executes semicolon-separated SQL statements in order.
// Statements use serializable autocommit unless enclosed by BEGIN and COMMIT.
func (engine *Engine) Execute(sql string) ([]executor.Result, error) {
	return engine.ExecuteContext(context.Background(), sql)
}

// ExecuteContext checks cancellation between planning, execution, and commit
// boundaries. Operators are currently cooperative at statement boundaries.
func (engine *Engine) ExecuteContext(ctx context.Context, sql string) ([]executor.Result, error) {
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
		if err := ctx.Err(); err != nil {
			if engine.active != nil {
				engine.failed = true
			}
			return results, err
		}
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
		if err == nil {
			err = ctx.Err()
		}
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

// Describe returns the typed result columns of one statement without executing
// it, for prepared-statement and wire-protocol metadata.
func (engine *Engine) Describe(sql string) ([]executor.ResultColumn, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closed {
		return nil, fmt.Errorf("sql engine: database is closed")
	}
	statement, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	if engine.failed {
		return nil, fmt.Errorf("current transaction is aborted; ROLLBACK is required")
	}
	transaction := engine.active
	if transaction == nil {
		transaction = engine.manager.Begin()
		defer transaction.Rollback()
	}
	_, binderValue, _, indexes, err := engine.components(transaction)
	if err != nil {
		return nil, err
	}
	bound, err := binderValue.Bind(statement)
	if err != nil {
		return nil, err
	}
	logical, err := plan.Build(bound)
	if err != nil {
		return nil, err
	}
	physical, err := plan.Optimize(logical, indexes)
	if err != nil {
		return nil, err
	}
	return executor.DescribeColumns(physical), nil
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

// DistributedSpans binds and optimizes one SQL statement, then exposes the
// range-local scan fragments that a distributed coordinator will schedule.
func (engine *Engine) DistributedSpans(sql string) ([]query.ScanSpan, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closed {
		return nil, fmt.Errorf("sql engine: database is closed")
	}
	statement, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	if engine.failed {
		return nil, fmt.Errorf("current transaction is aborted; ROLLBACK is required")
	}
	transaction := engine.active
	if transaction == nil {
		transaction = engine.manager.Begin()
		defer transaction.Rollback()
	}
	_, binderValue, _, indexes, err := engine.components(transaction)
	if err != nil {
		return nil, err
	}
	bound, err := binderValue.Bind(statement)
	if err != nil {
		return nil, err
	}
	logical, err := plan.Build(bound)
	if err != nil {
		return nil, err
	}
	physical, err := plan.Optimize(logical, indexes)
	if err != nil {
		return nil, err
	}
	return query.DeriveScanSpans(physical, engine.ranges.Descriptors())
}

// Catalog exposes an autocommit administrative catalog view.
func (engine *Engine) Catalog() *catalog.Catalog { return engine.catalog }

// Ranges exposes a read-mostly routing view for diagnostics and administrative
// split orchestration.
func (engine *Engine) Ranges() *ranges.Router { return engine.ranges }

// UpgradeStatus reports the durable active/target cluster versions and binary
// readiness of registered nodes.
func (engine *Engine) UpgradeStatus() upgrade.Status { return engine.upgrade.Status() }

func (engine *Engine) RegisterUpgradeNode(binary upgrade.Binary) error {
	return engine.upgrade.Register(binary)
}

func (engine *Engine) RemoveUpgradeNode(nodeID string) error {
	return engine.upgrade.Remove(nodeID)
}

func (engine *Engine) BeginUpgrade(target upgrade.Version) error {
	return engine.upgrade.Begin(target)
}

func (engine *Engine) AcknowledgeUpgrade(nodeID string, target upgrade.Version) error {
	return engine.upgrade.Acknowledge(nodeID, target)
}

func (engine *Engine) FinalizeUpgrade(target upgrade.Version) error {
	return engine.upgrade.Finalize(target)
}

func (engine *Engine) AbortUpgrade(target upgrade.Version) error {
	return engine.upgrade.Abort(target)
}

// ConsensusStatus reports the local member of the default range's Raft group.
func (engine *Engine) ConsensusStatus() raft.Status { return engine.raftNode.Status() }

// Diagnostics returns a consistent-enough operational snapshot without
// exposing WAL or table file internals.
func (engine *Engine) Diagnostics() Diagnostics {
	return Diagnostics{
		Storage: engine.store.Stats(), Consensus: engine.raftNode.Status(),
		Ranges: engine.ranges.Descriptors(), Upgrade: engine.upgrade.Status(),
	}
}

// Health verifies cancellation and a linearizable consensus read barrier.
func (engine *Engine) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	engine.mu.Lock()
	closed := engine.closed
	engine.mu.Unlock()
	if closed {
		return fmt.Errorf("sql engine: database is closed")
	}
	return engine.raftGroup.LinearizableRead()
}

// Checkpoint creates an online, self-contained backup. Explicit transactions
// must finish first so the checkpoint corresponds to a session-independent
// committed state.
func (engine *Engine) Checkpoint(destination string) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closed {
		return fmt.Errorf("sql engine: database is closed")
	}
	if engine.active != nil {
		return fmt.Errorf("sql engine: cannot checkpoint during an explicit transaction")
	}
	return engine.store.Checkpoint(destination)
}

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
