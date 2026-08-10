// Package binder resolves AST names and types against the persistent catalog.
package binder

import (
	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/types"
)

type Statement interface{ boundStatement() }

type ExpressionKind uint8

const (
	LiteralExpression ExpressionKind = iota + 1
	ColumnExpression
	UnaryExpression
	BinaryExpression
	FunctionExpression
)

type Expression struct {
	Kind      ExpressionKind
	Type      types.Type
	Nullable  bool
	Literal   types.Value
	ColumnID  uint32
	TableID   catalog.DescriptorID
	Name      string
	Operator  string
	Arguments []*Expression
}

type CreateDatabase struct{ Name string }
type CreateSchema struct {
	DatabaseID catalog.DescriptorID
	Name       string
}
type CreateTable struct {
	SchemaID     catalog.DescriptorID
	Name         string
	Columns      []codec.ColumnDescriptor
	PrimaryKey   []uint32
	UniqueColumn []uint32
}
type CreateIndex struct {
	Table     catalog.TableDescriptor
	Name      string
	ColumnIDs []uint32
	Unique    bool
}
type DropTable struct {
	Table    catalog.TableDescriptor
	Missing  string
	IfExists bool
}
type Insert struct {
	Table catalog.TableDescriptor
	Rows  []codec.Row
}
type Projection struct {
	Expression *Expression
	Alias      string
}
type Ordering struct {
	Expression *Expression
	Descending bool
}
type Select struct {
	Table      catalog.TableDescriptor
	Joins      []Join
	Projection []Projection
	Filter     *Expression
	Ordering   []Ordering
	Limit      *int64
}
type Join struct {
	Table catalog.TableDescriptor
	On    *Expression
}
type Assignment struct {
	ColumnID uint32
	Value    *Expression
}
type Update struct {
	Table       catalog.TableDescriptor
	Assignments []Assignment
	Filter      *Expression
}
type Delete struct {
	Table  catalog.TableDescriptor
	Filter *Expression
}
type ShowTables struct{ SchemaID catalog.DescriptorID }
type DescribeTable struct{ Table catalog.TableDescriptor }
type Begin struct{}
type Commit struct{}
type Rollback struct{}

func (CreateDatabase) boundStatement() {}
func (CreateSchema) boundStatement()   {}
func (CreateTable) boundStatement()    {}
func (CreateIndex) boundStatement()    {}
func (DropTable) boundStatement()      {}
func (Insert) boundStatement()         {}
func (Select) boundStatement()         {}
func (Update) boundStatement()         {}
func (Delete) boundStatement()         {}
func (ShowTables) boundStatement()     {}
func (DescribeTable) boundStatement()  {}
func (Begin) boundStatement()          {}
func (Commit) boundStatement()         {}
func (Rollback) boundStatement()       {}
