// Package plan builds explicit logical and physical query plans.
package plan

import (
	"fmt"
	"strings"

	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/sql/binder"
)

type Kind string

const (
	ScanKind        Kind = "Scan"
	JoinKind        Kind = "NestedLoopJoin"
	FilterKind      Kind = "Filter"
	SortKind        Kind = "Sort"
	ProjectKind     Kind = "Project"
	LimitKind       Kind = "Limit"
	AggregateKind   Kind = "Aggregate"
	InsertKind      Kind = "Insert"
	UpdateKind      Kind = "Update"
	DeleteKind      Kind = "Delete"
	CreateKind      Kind = "Create"
	DropKind        Kind = "Drop"
	ShowKind        Kind = "Show"
	DescribeKind    Kind = "Describe"
	TransactionKind Kind = "Transaction"
)

type LogicalNode struct {
	Kind        Kind
	Input       *LogicalNode
	Right       *LogicalNode
	Table       catalog.TableDescriptor
	Expression  *binder.Expression
	Projection  []binder.Projection
	Ordering    []binder.Ordering
	Limit       *int64
	Rows        []codec.Row
	Assignments []binder.Assignment
	Statement   binder.Statement
}

type LogicalPlan struct{ Root *LogicalNode }

type AccessPath string

const FullTableScan AccessPath = "full-table-scan"

type PhysicalNode struct {
	Kind        Kind
	Input       *PhysicalNode
	Right       *PhysicalNode
	Table       catalog.TableDescriptor
	Access      AccessPath
	Expression  *binder.Expression
	Projection  []binder.Projection
	Ordering    []binder.Ordering
	Limit       *int64
	Rows        []codec.Row
	Assignments []binder.Assignment
	Statement   binder.Statement
}

type PhysicalPlan struct{ Root *PhysicalNode }

func Build(statement binder.Statement) (LogicalPlan, error) {
	switch value := statement.(type) {
	case binder.Select:
		root := &LogicalNode{Kind: ScanKind, Table: value.Table}
		for _, join := range value.Joins {
			root = &LogicalNode{
				Kind: JoinKind, Input: root,
				Right:      &LogicalNode{Kind: ScanKind, Table: join.Table},
				Expression: join.On,
			}
		}
		if value.Filter != nil {
			root = &LogicalNode{Kind: FilterKind, Input: root, Table: value.Table, Expression: value.Filter}
		}
		hasAggregate := false
		for _, projection := range value.Projection {
			if projection.Expression.Kind == binder.FunctionExpression && (projection.Expression.Name == "count" || projection.Expression.Name == "sum") {
				hasAggregate = true
				continue
			}
			if hasAggregate {
				return LogicalPlan{}, fmt.Errorf("sql plan: global aggregate query cannot mix aggregate and non-aggregate projections")
			}
		}
		if hasAggregate {
			for _, projection := range value.Projection {
				if projection.Expression.Kind != binder.FunctionExpression || (projection.Expression.Name != "count" && projection.Expression.Name != "sum") {
					return LogicalPlan{}, fmt.Errorf("sql plan: global aggregate query cannot mix aggregate and non-aggregate projections")
				}
			}
			if len(value.Ordering) > 0 {
				return LogicalPlan{}, fmt.Errorf("sql plan: ORDER BY on global aggregates is not implemented")
			}
			root = &LogicalNode{Kind: AggregateKind, Input: root, Table: value.Table, Projection: value.Projection}
		} else {
			if len(value.Ordering) > 0 {
				root = &LogicalNode{Kind: SortKind, Input: root, Table: value.Table, Ordering: value.Ordering}
			}
			root = &LogicalNode{Kind: ProjectKind, Input: root, Table: value.Table, Projection: value.Projection}
		}
		if value.Limit != nil {
			root = &LogicalNode{Kind: LimitKind, Input: root, Table: value.Table, Limit: value.Limit}
		}
		return LogicalPlan{Root: root}, nil
	case binder.Insert:
		return LogicalPlan{Root: &LogicalNode{Kind: InsertKind, Table: value.Table, Rows: value.Rows, Statement: value}}, nil
	case binder.Update:
		return LogicalPlan{Root: &LogicalNode{Kind: UpdateKind, Table: value.Table, Expression: value.Filter, Assignments: value.Assignments, Statement: value}}, nil
	case binder.Delete:
		return LogicalPlan{Root: &LogicalNode{Kind: DeleteKind, Table: value.Table, Expression: value.Filter, Statement: value}}, nil
	case binder.CreateDatabase, binder.CreateSchema, binder.CreateTable, binder.CreateIndex:
		return LogicalPlan{Root: &LogicalNode{Kind: CreateKind, Statement: statement}}, nil
	case binder.DropTable:
		return LogicalPlan{Root: &LogicalNode{Kind: DropKind, Statement: statement}}, nil
	case binder.ShowTables:
		return LogicalPlan{Root: &LogicalNode{Kind: ShowKind, Statement: statement}}, nil
	case binder.DescribeTable:
		return LogicalPlan{Root: &LogicalNode{Kind: DescribeKind, Statement: statement}}, nil
	case binder.Begin, binder.Commit, binder.Rollback:
		return LogicalPlan{Root: &LogicalNode{Kind: TransactionKind, Statement: statement}}, nil
	default:
		return LogicalPlan{}, fmt.Errorf("sql plan: unsupported statement %T", statement)
	}
}

// Physicalize selects the initial physical access path. Milestone 6 replaces
// FullTableScan when a compatible primary or secondary index span exists.
func Physicalize(logical LogicalPlan) (PhysicalPlan, error) {
	if logical.Root == nil {
		return PhysicalPlan{}, fmt.Errorf("sql plan: logical plan has no root")
	}
	return PhysicalPlan{Root: physicalizeNode(logical.Root)}, nil
}

func physicalizeNode(logical *LogicalNode) *PhysicalNode {
	if logical == nil {
		return nil
	}
	physical := &PhysicalNode{
		Kind: logical.Kind, Table: logical.Table, Expression: logical.Expression,
		Projection: logical.Projection, Ordering: logical.Ordering, Limit: logical.Limit,
		Rows: logical.Rows, Assignments: logical.Assignments, Statement: logical.Statement,
	}
	if logical.Kind == ScanKind {
		physical.Access = FullTableScan
	}
	physical.Input = physicalizeNode(logical.Input)
	physical.Right = physicalizeNode(logical.Right)
	return physical
}

func Explain(plan PhysicalPlan) string {
	var result strings.Builder
	explainNode(&result, plan.Root, 0)
	return strings.TrimSuffix(result.String(), "\n")
}

func explainNode(result *strings.Builder, node *PhysicalNode, depth int) {
	if node == nil {
		return
	}
	result.WriteString(strings.Repeat("  ", depth))
	result.WriteString(string(node.Kind))
	if node.Table.Name != "" {
		fmt.Fprintf(result, " table=%s", node.Table.Name)
	}
	if node.Access != "" {
		fmt.Fprintf(result, " access=%s", node.Access)
	}
	if node.Limit != nil {
		fmt.Fprintf(result, " rows=%d", *node.Limit)
	}
	result.WriteByte('\n')
	explainNode(result, node.Input, depth+1)
	explainNode(result, node.Right, depth+1)
}
