package plan_test

import (
	"strings"
	"testing"

	"pebbledb/catalog"
	"pebbledb/sql/binder"
	"pebbledb/sql/plan"
)

func TestSelectPlanOperatorOrder(t *testing.T) {
	limit := int64(5)
	table := catalog.TableDescriptor{Name: "users"}
	bound := binder.Select{
		Table:      table,
		Projection: []binder.Projection{{Expression: &binder.Expression{Kind: binder.ColumnExpression}}},
		Filter:     &binder.Expression{}, Ordering: []binder.Ordering{{Expression: &binder.Expression{}}}, Limit: &limit,
	}
	logical, err := plan.Build(bound)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	physical, err := plan.Physicalize(logical)
	if err != nil {
		t.Fatalf("physicalize: %v", err)
	}
	explanation := plan.Explain(physical)
	for _, fragment := range []string{"Limit", "Project", "Sort", "Filter", "Scan table=users access=full-table-scan"} {
		if !strings.Contains(explanation, fragment) {
			t.Fatalf("plan does not contain %q:\n%s", fragment, explanation)
		}
	}
}

func TestAggregatePlanRejectsMixedProjection(t *testing.T) {
	table := catalog.TableDescriptor{Name: "users"}
	_, err := plan.Build(binder.Select{Table: table, Projection: []binder.Projection{
		{Expression: &binder.Expression{Kind: binder.FunctionExpression, Name: "count"}},
		{Expression: &binder.Expression{Kind: binder.ColumnExpression}},
	}})
	if err == nil {
		t.Fatal("expected mixed aggregate projection to fail")
	}
}
