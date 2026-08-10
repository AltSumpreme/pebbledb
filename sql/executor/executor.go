// Package executor runs physical plans against the catalog and row store.
package executor

import (
	"errors"
	"fmt"
	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/sql/binder"
	"pebbledb/sql/plan"
	"pebbledb/storage/rowstore"
	"pebbledb/types"
	"sort"
	"sync"
)

type ResultColumn struct {
	Name string
	Type types.Type
}

type Result struct {
	Columns      []ResultColumn
	Rows         [][]types.Value
	RowsAffected int64
	Message      string
	Plan         string
}

type Executor struct {
	mu      sync.Mutex
	catalog *catalog.Catalog
	rows    *rowstore.Store
}

func New(catalogValue *catalog.Catalog, rows *rowstore.Store) (*Executor, error) {
	if catalogValue == nil || rows == nil {
		return nil, fmt.Errorf("sql executor: catalog and row store are required")
	}
	return &Executor{catalog: catalogValue, rows: rows}, nil
}

func (executor *Executor) Execute(physical plan.PhysicalPlan) (Result, error) {
	if physical.Root == nil {
		return Result{}, fmt.Errorf("sql executor: plan has no root")
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	result := Result{Plan: plan.Explain(physical)}
	switch physical.Root.Kind {
	case plan.ScanKind, plan.JoinKind, plan.FilterKind, plan.SortKind, plan.ProjectKind, plan.LimitKind, plan.AggregateKind:
		rows, err := executor.executeOperator(physical.Root)
		if err != nil {
			return Result{}, err
		}
		result.Rows = make([][]types.Value, len(rows))
		for index := range rows {
			result.Rows[index] = rows[index].output
		}
		result.Columns = resultColumns(physical.Root)
		result.Message = "SELECT"
		return result, nil
	case plan.InsertKind:
		statement := physical.Root.Statement.(binder.Insert)
		if err := executor.insert(statement); err != nil {
			return Result{}, err
		}
		result.RowsAffected, result.Message = int64(len(statement.Rows)), "INSERT"
		return result, nil
	case plan.UpdateKind:
		count, err := executor.update(physical.Root)
		result.RowsAffected, result.Message = count, "UPDATE"
		return result, err
	case plan.DeleteKind:
		count, err := executor.delete(physical.Root)
		result.RowsAffected, result.Message = count, "DELETE"
		return result, err
	case plan.CreateKind:
		message, err := executor.create(physical.Root.Statement)
		result.Message = message
		return result, err
	case plan.DropKind:
		statement := physical.Root.Statement.(binder.DropTable)
		if statement.Missing != "" && statement.IfExists {
			result.Message = "DROP TABLE"
			return result, nil
		}
		result.Message = "DROP TABLE"
		return result, executor.catalog.DropTable(statement.Table.ID)
	case plan.ShowKind:
		statement := physical.Root.Statement.(binder.ShowTables)
		tables, err := executor.catalog.ListTables(statement.SchemaID)
		if err != nil {
			return Result{}, err
		}
		result.Columns = []ResultColumn{{Name: "table_name", Type: types.TextType()}}
		for _, table := range tables {
			name, _ := types.TextValue(table.Name)
			result.Rows = append(result.Rows, []types.Value{name})
		}
		result.Message = "SHOW TABLES"
		return result, nil
	case plan.DescribeKind:
		return executor.describe(physical.Root.Statement.(binder.DescribeTable).Table, result.Plan)
	case plan.TransactionKind:
		return Result{}, fmt.Errorf("sql executor: explicit transactions require Milestone 7")
	default:
		return Result{}, fmt.Errorf("sql executor: unsupported physical node %s", physical.Root.Kind)
	}
}

type record struct {
	values map[cellKey]types.Value
	output []types.Value
}

type cellKey struct {
	tableID  catalog.DescriptorID
	columnID uint32
}

func (executor *Executor) executeOperator(node *plan.PhysicalNode) ([]record, error) {
	switch node.Kind {
	case plan.ScanKind:
		stored, err := executor.rows.Scan(node.Table)
		if err != nil {
			return nil, err
		}
		result := make([]record, len(stored))
		for index := range stored {
			result[index] = record{values: rowCells(node.Table.ID, stored[index].Row.Values)}
		}
		return result, nil
	case plan.JoinKind:
		left, err := executor.executeOperator(node.Input)
		if err != nil {
			return nil, err
		}
		right, err := executor.executeOperator(node.Right)
		if err != nil {
			return nil, err
		}
		result := make([]record, 0)
		for _, leftRow := range left {
			for _, rightRow := range right {
				combined := make(map[cellKey]types.Value, len(leftRow.values)+len(rightRow.values))
				for key, value := range leftRow.values {
					combined[key] = value
				}
				for key, value := range rightRow.values {
					combined[key] = value
				}
				matches, err := matchesFilter(node.Expression, combined)
				if err != nil {
					return nil, err
				}
				if matches {
					result = append(result, record{values: combined})
				}
			}
		}
		return result, nil
	case plan.FilterKind:
		input, err := executor.executeOperator(node.Input)
		if err != nil {
			return nil, err
		}
		result := make([]record, 0, len(input))
		for _, current := range input {
			value, err := evaluate(node.Expression, current.values)
			if err != nil {
				return nil, err
			}
			if value.IsNull() {
				continue
			}
			matches, err := value.AsBool()
			if err != nil {
				return nil, err
			}
			if matches {
				result = append(result, current)
			}
		}
		return result, nil
	case plan.SortKind:
		input, err := executor.executeOperator(node.Input)
		if err != nil {
			return nil, err
		}
		var compareErr error
		sort.SliceStable(input, func(i, j int) bool {
			if compareErr != nil {
				return false
			}
			for _, ordering := range node.Ordering {
				left, err := evaluate(ordering.Expression, input[i].values)
				if err != nil {
					compareErr = err
					return false
				}
				right, err := evaluate(ordering.Expression, input[j].values)
				if err != nil {
					compareErr = err
					return false
				}
				comparison, err := compareValues(left, right)
				if err != nil {
					compareErr = err
					return false
				}
				if comparison == 0 {
					continue
				}
				if ordering.Descending {
					return comparison > 0
				}
				return comparison < 0
			}
			return false
		})
		return input, compareErr
	case plan.ProjectKind:
		input, err := executor.executeOperator(node.Input)
		if err != nil {
			return nil, err
		}
		for index := range input {
			for _, projection := range node.Projection {
				value, err := evaluate(projection.Expression, input[index].values)
				if err != nil {
					return nil, err
				}
				input[index].output = append(input[index].output, value)
			}
		}
		return input, nil
	case plan.AggregateKind:
		input, err := executor.executeOperator(node.Input)
		if err != nil {
			return nil, err
		}
		output := make([]types.Value, 0, len(node.Projection))
		for _, projection := range node.Projection {
			value, err := aggregate(projection.Expression, input)
			if err != nil {
				return nil, err
			}
			output = append(output, value)
		}
		return []record{{output: output}}, nil
	case plan.LimitKind:
		input, err := executor.executeOperator(node.Input)
		if err != nil {
			return nil, err
		}
		if node.Limit == nil || *node.Limit >= int64(len(input)) {
			return input, nil
		}
		return input[:*node.Limit], nil
	default:
		return nil, fmt.Errorf("sql executor: %s is not a row operator", node.Kind)
	}
}

func (executor *Executor) insert(statement binder.Insert) error {
	keys := make(map[string]struct{}, len(statement.Rows))
	for _, row := range statement.Rows {
		key, err := rowstore.KeyFromRow(statement.Table, row)
		if err != nil {
			return err
		}
		if _, duplicate := keys[string(key)]; duplicate {
			return rowstore.ErrDuplicateKey
		}
		keys[string(key)] = struct{}{}
		primary := primaryValues(statement.Table, row)
		if _, err := executor.rows.Get(statement.Table, primary); err == nil {
			return rowstore.ErrDuplicateKey
		} else if !errors.Is(err, rowstore.ErrRowNotFound) {
			return err
		}
	}
	for _, row := range statement.Rows {
		if err := executor.rows.Insert(statement.Table, row); err != nil {
			return err
		}
	}
	return nil
}

func (executor *Executor) update(node *plan.PhysicalNode) (int64, error) {
	stored, err := executor.rows.Scan(node.Table)
	if err != nil {
		return 0, err
	}
	primary := make(map[uint32]struct{}, len(node.Table.Schema.PrimaryKey))
	for _, id := range node.Table.Schema.PrimaryKey {
		primary[id] = struct{}{}
	}
	for _, assignment := range node.Assignments {
		if _, isPrimary := primary[assignment.ColumnID]; isPrimary {
			return 0, fmt.Errorf("sql executor: updating primary-key columns requires transactional key moves")
		}
	}
	var affected int64
	for _, current := range stored {
		currentCells := rowCells(node.Table.ID, current.Row.Values)
		matches, err := matchesFilter(node.Expression, currentCells)
		if err != nil {
			return affected, err
		}
		if !matches {
			continue
		}
		updated := codec.Row{SchemaVersion: current.Row.SchemaVersion, Values: cloneValues(current.Row.Values)}
		for _, assignment := range node.Assignments {
			value, err := evaluate(assignment.Value, currentCells)
			if err != nil {
				return affected, err
			}
			updated.Values[assignment.ColumnID] = value
		}
		if err := executor.rows.Replace(node.Table, updated); err != nil {
			return affected, err
		}
		affected++
	}
	return affected, nil
}

func (executor *Executor) delete(node *plan.PhysicalNode) (int64, error) {
	stored, err := executor.rows.Scan(node.Table)
	if err != nil {
		return 0, err
	}
	var affected int64
	for _, current := range stored {
		matches, err := matchesFilter(node.Expression, rowCells(node.Table.ID, current.Row.Values))
		if err != nil {
			return affected, err
		}
		if matches {
			if err := executor.rows.Delete(node.Table, current.Row); err != nil {
				return affected, err
			}
			affected++
		}
	}
	return affected, nil
}

func (executor *Executor) create(statement binder.Statement) (string, error) {
	switch value := statement.(type) {
	case binder.CreateDatabase:
		_, err := executor.catalog.CreateDatabase(value.Name)
		return "CREATE DATABASE", err
	case binder.CreateSchema:
		_, err := executor.catalog.CreateSchema(value.DatabaseID, value.Name)
		return "CREATE SCHEMA", err
	case binder.CreateTable:
		table, err := executor.catalog.CreateTable(value.SchemaID, value.Name, value.Columns, value.PrimaryKey)
		if err != nil {
			return "", err
		}
		for _, columnID := range value.UniqueColumn {
			columnName := fmt.Sprintf("column_%d", columnID)
			for _, column := range value.Columns {
				if column.ID == columnID {
					columnName = column.Name
				}
			}
			if _, err := executor.catalog.AddIndex(table.ID, value.Name+"_"+columnName+"_key", []uint32{columnID}, true); err != nil {
				return "", err
			}
		}
		return "CREATE TABLE", nil
	case binder.CreateIndex:
		_, err := executor.catalog.AddIndex(value.Table.ID, value.Name, value.ColumnIDs, value.Unique)
		return "CREATE INDEX", err
	default:
		return "", fmt.Errorf("sql executor: unsupported CREATE %T", statement)
	}
}

func (executor *Executor) describe(table catalog.TableDescriptor, explanation string) (Result, error) {
	result := Result{
		Columns: []ResultColumn{{Name: "column_name", Type: types.TextType()}, {Name: "data_type", Type: types.TextType()}, {Name: "nullable", Type: types.BoolType()}, {Name: "primary_key", Type: types.BoolType()}},
		Message: "DESCRIBE", Plan: explanation,
	}
	primary := make(map[uint32]struct{}, len(table.Schema.PrimaryKey))
	for _, id := range table.Schema.PrimaryKey {
		primary[id] = struct{}{}
	}
	for _, column := range table.Schema.Columns {
		name, _ := types.TextValue(column.Name)
		typeName, _ := types.TextValue(column.Type.String())
		_, isPrimary := primary[column.ID]
		result.Rows = append(result.Rows, []types.Value{name, typeName, types.BoolValue(column.Nullable), types.BoolValue(isPrimary)})
	}
	return result, nil
}

func resultColumns(node *plan.PhysicalNode) []ResultColumn {
	for current := node; current != nil; current = current.Input {
		if current.Kind != plan.ProjectKind && current.Kind != plan.AggregateKind {
			continue
		}
		result := make([]ResultColumn, 0, len(current.Projection))
		for index, projection := range current.Projection {
			name := projection.Alias
			if name == "" {
				name = projection.Expression.Name
			}
			if name == "" {
				name = fmt.Sprintf("column_%d", index+1)
			}
			result = append(result, ResultColumn{Name: name, Type: projection.Expression.Type})
		}
		return result
	}
	return nil
}

func matchesFilter(filter *binder.Expression, values map[cellKey]types.Value) (bool, error) {
	if filter == nil {
		return true, nil
	}
	value, err := evaluate(filter, values)
	if err != nil || value.IsNull() {
		return false, err
	}
	return value.AsBool()
}

func rowCells(tableID catalog.DescriptorID, values map[uint32]types.Value) map[cellKey]types.Value {
	result := make(map[cellKey]types.Value, len(values))
	for columnID, value := range values {
		result[cellKey{tableID: tableID, columnID: columnID}] = value
	}
	return result
}

func cloneValues(values map[uint32]types.Value) map[uint32]types.Value {
	result := make(map[uint32]types.Value, len(values))
	for id, value := range values {
		result[id] = value
	}
	return result
}

func primaryValues(table catalog.TableDescriptor, row codec.Row) []types.Value {
	result := make([]types.Value, 0, len(table.Schema.PrimaryKey))
	for _, id := range table.Schema.PrimaryKey {
		result = append(result, row.Values[id])
	}
	return result
}
