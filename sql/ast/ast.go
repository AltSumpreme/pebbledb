// Package ast defines PebbleDB's parser output without catalog dependencies.
package ast

type Statement interface{ statementNode() }
type Expression interface{ expressionNode() }

type Name struct {
	Parts []string
}

func (name Name) String() string {
	if len(name.Parts) == 0 {
		return ""
	}
	result := name.Parts[0]
	for _, part := range name.Parts[1:] {
		result += "." + part
	}
	return result
}

type ColumnDefinition struct {
	Name       string
	TypeName   string
	Nullable   bool
	PrimaryKey bool
	Unique     bool
}

type CreateDatabase struct{ Name string }
type CreateSchema struct{ Name Name }
type CreateTable struct {
	Name       Name
	Columns    []ColumnDefinition
	PrimaryKey []string
}
type CreateIndex struct {
	Name    string
	Table   Name
	Columns []string
	Unique  bool
}
type DropTable struct {
	Name     Name
	IfExists bool
}

type Insert struct {
	Table   Name
	Columns []string
	Rows    [][]Expression
}

type SelectItem struct {
	Expression Expression
	Alias      string
}
type OrderBy struct {
	Expression Expression
	Descending bool
}
type Join struct {
	Table Name
	On    Expression
}
type Select struct {
	Items   []SelectItem
	From    Name
	Joins   []Join
	Where   Expression
	OrderBy []OrderBy
	Limit   *int64
}
type Assignment struct {
	Column string
	Value  Expression
}
type Update struct {
	Table       Name
	Assignments []Assignment
	Where       Expression
}
type Delete struct {
	Table Name
	Where Expression
}

type ShowTables struct{}
type DescribeTable struct{ Name Name }
type Begin struct{}
type Commit struct{}
type Rollback struct{}

func (CreateDatabase) statementNode() {}
func (CreateSchema) statementNode()   {}
func (CreateTable) statementNode()    {}
func (CreateIndex) statementNode()    {}
func (DropTable) statementNode()      {}
func (Insert) statementNode()         {}
func (Select) statementNode()         {}
func (Update) statementNode()         {}
func (Delete) statementNode()         {}
func (ShowTables) statementNode()     {}
func (DescribeTable) statementNode()  {}
func (Begin) statementNode()          {}
func (Commit) statementNode()         {}
func (Rollback) statementNode()       {}

type LiteralKind uint8

const (
	NullLiteral LiteralKind = iota + 1
	BoolLiteral
	NumberLiteral
	StringLiteral
)

type Literal struct {
	Kind LiteralKind
	Raw  string
}
type ColumnReference struct{ Name Name }
type Star struct{}
type UnaryExpression struct {
	Operator string
	Value    Expression
}
type BinaryExpression struct {
	Left     Expression
	Operator string
	Right    Expression
}
type FunctionCall struct {
	Name      string
	Arguments []Expression
}

func (Literal) expressionNode()          {}
func (ColumnReference) expressionNode()  {}
func (Star) expressionNode()             {}
func (UnaryExpression) expressionNode()  {}
func (BinaryExpression) expressionNode() {}
func (FunctionCall) expressionNode()     {}
