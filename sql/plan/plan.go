// Package plan builds explicit logical and physical query plans.
package plan

import (
	"fmt"
	"strings"

	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/sql/binder"
	"pebbledb/types"
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

const (
	FullTableScan      AccessPath = "full-table-scan"
	PrimaryKeyLookup   AccessPath = "primary-key-lookup"
	SecondaryIndexScan AccessPath = "secondary-index-scan"
)

type StatsProvider interface {
	IndexStats(table catalog.TableDescriptor, index catalog.IndexDescriptor) (entries uint64, distinct uint64, err error)
}

type PhysicalNode struct {
	Kind         Kind
	Input        *PhysicalNode
	Right        *PhysicalNode
	Table        catalog.TableDescriptor
	Access       AccessPath
	Index        *catalog.IndexDescriptor
	LookupValues []types.Value
	Expression   *binder.Expression
	Projection   []binder.Projection
	Ordering     []binder.Ordering
	Limit        *int64
	Rows         []codec.Row
	Assignments  []binder.Assignment
	Statement    binder.Statement
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
	return Optimize(logical, nil)
}

// Optimize chooses equality lookup paths using catalog keys and current index
// cardinality. Primary-key equality always wins; otherwise the most selective
// eligible single-column secondary index is chosen.
func Optimize(logical LogicalPlan, statistics StatsProvider) (PhysicalPlan, error) {
	if logical.Root == nil {
		return PhysicalPlan{}, fmt.Errorf("sql plan: logical plan has no root")
	}
	physical := PhysicalPlan{Root: physicalizeNode(logical.Root)}
	candidates := make(map[catalog.DescriptorID][]lookupCandidate)
	collectCandidates(physical.Root, candidates)
	if err := chooseAccessPaths(physical.Root, candidates, statistics); err != nil {
		return PhysicalPlan{}, err
	}
	return physical, nil
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

type lookupCandidate struct {
	columnID uint32
	value    types.Value
}

func collectCandidates(node *PhysicalNode, candidates map[catalog.DescriptorID][]lookupCandidate) {
	if node == nil {
		return
	}
	if node.Kind == FilterKind {
		collectExpressionCandidates(node.Expression, candidates)
	}
	collectCandidates(node.Input, candidates)
	collectCandidates(node.Right, candidates)
}

func collectExpressionCandidates(expression *binder.Expression, candidates map[catalog.DescriptorID][]lookupCandidate) {
	if expression == nil || expression.Kind != binder.BinaryExpression {
		return
	}
	if expression.Operator == "AND" {
		for _, argument := range expression.Arguments {
			collectExpressionCandidates(argument, candidates)
		}
		return
	}
	if expression.Operator != "=" || len(expression.Arguments) != 2 {
		return
	}
	left, right := expression.Arguments[0], expression.Arguments[1]
	if left.Kind == binder.LiteralExpression && right.Kind == binder.ColumnExpression {
		left, right = right, left
	}
	if left.Kind != binder.ColumnExpression || right.Kind != binder.LiteralExpression || right.Literal.IsNull() {
		return
	}
	candidates[left.TableID] = append(candidates[left.TableID], lookupCandidate{columnID: left.ColumnID, value: right.Literal})
}

func chooseAccessPaths(node *PhysicalNode, candidates map[catalog.DescriptorID][]lookupCandidate, statistics StatsProvider) error {
	if node == nil {
		return nil
	}
	if node.Kind == ScanKind {
		for _, candidate := range candidates[node.Table.ID] {
			if len(node.Table.Schema.PrimaryKey) == 1 && node.Table.Schema.PrimaryKey[0] == candidate.columnID {
				node.Access = PrimaryKeyLookup
				node.LookupValues = []types.Value{candidate.value}
				return nil
			}
		}
		bestCost := ^uint64(0)
		for _, candidate := range candidates[node.Table.ID] {
			for indexPosition := range node.Table.Indexes {
				index := node.Table.Indexes[indexPosition]
				if len(index.ColumnIDs) != 1 || index.ColumnIDs[0] != candidate.columnID {
					continue
				}
				cost := uint64(1)
				if statistics != nil {
					entries, distinct, err := statistics.IndexStats(node.Table, index)
					if err != nil {
						return err
					}
					if distinct > 0 {
						cost = entries / distinct
						if cost == 0 {
							cost = 1
						}
					}
				}
				if cost < bestCost {
					indexCopy := index
					node.Access = SecondaryIndexScan
					node.Index = &indexCopy
					node.LookupValues = []types.Value{candidate.value}
					bestCost = cost
				}
			}
		}
	}
	if err := chooseAccessPaths(node.Input, candidates, statistics); err != nil {
		return err
	}
	return chooseAccessPaths(node.Right, candidates, statistics)
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
	if node.Index != nil {
		fmt.Fprintf(result, " index=%s", node.Index.Name)
	}
	if node.Limit != nil {
		fmt.Fprintf(result, " rows=%d", *node.Limit)
	}
	result.WriteByte('\n')
	explainNode(result, node.Input, depth+1)
	explainNode(result, node.Right, depth+1)
}
