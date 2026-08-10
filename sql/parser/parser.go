// Package parser turns lexer tokens into a SQL AST.
package parser

import (
	"fmt"
	"strconv"
	"strings"

	"pebbledb/sql/ast"
	"pebbledb/sql/lexer"
)

func Parse(input string) (ast.Statement, error) {
	statements, err := ParseStatements(input)
	if err != nil {
		return nil, err
	}
	if len(statements) != 1 {
		return nil, fmt.Errorf("sql parser: expected one statement, got %d", len(statements))
	}
	return statements[0], nil
}

func ParseStatements(input string) ([]ast.Statement, error) {
	tokens, err := lexer.Tokenize(input)
	if err != nil {
		return nil, err
	}
	parser := &Parser{tokens: tokens}
	var statements []ast.Statement
	for parser.peek().Kind != lexer.EOF {
		for parser.match(lexer.Semicolon) {
		}
		if parser.peek().Kind == lexer.EOF {
			break
		}
		statement, err := parser.parseStatement()
		if err != nil {
			return nil, err
		}
		statements = append(statements, statement)
		if parser.peek().Kind != lexer.EOF && !parser.match(lexer.Semicolon) {
			return nil, parser.errorf("expected semicolon or end of input")
		}
	}
	return statements, nil
}

type Parser struct {
	tokens []lexer.Token
	index  int
}

func (parser *Parser) parseStatement() (ast.Statement, error) {
	switch {
	case parser.matchKeyword("create"):
		return parser.parseCreate()
	case parser.matchKeyword("drop"):
		return parser.parseDrop()
	case parser.matchKeyword("insert"):
		return parser.parseInsert()
	case parser.matchKeyword("select"):
		return parser.parseSelect()
	case parser.matchKeyword("update"):
		return parser.parseUpdate()
	case parser.matchKeyword("delete"):
		return parser.parseDelete()
	case parser.matchKeyword("show"):
		if err := parser.expectKeyword("tables"); err != nil {
			return nil, err
		}
		return ast.ShowTables{}, nil
	case parser.matchKeyword("describe") || parser.matchKeyword("desc"):
		name, err := parser.parseName()
		return ast.DescribeTable{Name: name}, err
	case parser.matchKeyword("begin"):
		_ = parser.matchKeyword("transaction")
		return ast.Begin{}, nil
	case parser.matchKeyword("commit"):
		return ast.Commit{}, nil
	case parser.matchKeyword("rollback"):
		return ast.Rollback{}, nil
	default:
		return nil, parser.errorf("expected a SQL statement")
	}
}

func (parser *Parser) parseCreate() (ast.Statement, error) {
	switch {
	case parser.matchKeyword("database"):
		name, err := parser.expectIdentifier("database name")
		return ast.CreateDatabase{Name: name}, err
	case parser.matchKeyword("schema"):
		name, err := parser.parseName()
		return ast.CreateSchema{Name: name}, err
	case parser.matchKeyword("table"):
		return parser.parseCreateTable()
	default:
		unique := parser.matchKeyword("unique")
		if !parser.matchKeyword("index") {
			return nil, parser.errorf("expected DATABASE, SCHEMA, TABLE, or INDEX after CREATE")
		}
		return parser.parseCreateIndex(unique)
	}
}

func (parser *Parser) parseCreateTable() (ast.Statement, error) {
	name, err := parser.parseName()
	if err != nil {
		return nil, err
	}
	if err := parser.expect(lexer.LeftParen, "("); err != nil {
		return nil, err
	}
	statement := ast.CreateTable{Name: name}
	for {
		if parser.matchKeyword("primary") {
			if err := parser.expectKeyword("key"); err != nil {
				return nil, err
			}
			columns, err := parser.parseIdentifierList()
			if err != nil {
				return nil, err
			}
			statement.PrimaryKey = append(statement.PrimaryKey, columns...)
		} else {
			column, err := parser.parseColumnDefinition()
			if err != nil {
				return nil, err
			}
			statement.Columns = append(statement.Columns, column)
			if column.PrimaryKey {
				statement.PrimaryKey = append(statement.PrimaryKey, column.Name)
			}
		}
		if parser.match(lexer.RightParen) {
			break
		}
		if err := parser.expect(lexer.Comma, ","); err != nil {
			return nil, err
		}
	}
	if len(statement.Columns) == 0 {
		return nil, parser.errorf("CREATE TABLE requires at least one column")
	}
	return statement, nil
}

func (parser *Parser) parseColumnDefinition() (ast.ColumnDefinition, error) {
	name, err := parser.expectIdentifier("column name")
	if err != nil {
		return ast.ColumnDefinition{}, err
	}
	typeName, err := parser.expectIdentifier("column type")
	if err != nil {
		return ast.ColumnDefinition{}, err
	}
	if parser.match(lexer.LeftParen) {
		precision, err := parser.expectKind(lexer.Number, "type precision")
		if err != nil {
			return ast.ColumnDefinition{}, err
		}
		typeName += "(" + precision.Literal
		if parser.match(lexer.Comma) {
			scale, err := parser.expectKind(lexer.Number, "type scale")
			if err != nil {
				return ast.ColumnDefinition{}, err
			}
			typeName += "," + scale.Literal
		}
		if err := parser.expect(lexer.RightParen, ")"); err != nil {
			return ast.ColumnDefinition{}, err
		}
		typeName += ")"
	}
	column := ast.ColumnDefinition{Name: name, TypeName: typeName, Nullable: true}
	for {
		switch {
		case parser.matchKeyword("not"):
			if err := parser.expectKeyword("null"); err != nil {
				return ast.ColumnDefinition{}, err
			}
			column.Nullable = false
		case parser.matchKeyword("null"):
			column.Nullable = true
		case parser.matchKeyword("primary"):
			if err := parser.expectKeyword("key"); err != nil {
				return ast.ColumnDefinition{}, err
			}
			column.PrimaryKey, column.Nullable = true, false
		case parser.matchKeyword("unique"):
			column.Unique = true
		default:
			return column, nil
		}
	}
}

func (parser *Parser) parseCreateIndex(unique bool) (ast.Statement, error) {
	name, err := parser.expectIdentifier("index name")
	if err != nil {
		return nil, err
	}
	if err := parser.expectKeyword("on"); err != nil {
		return nil, err
	}
	table, err := parser.parseName()
	if err != nil {
		return nil, err
	}
	columns, err := parser.parseIdentifierList()
	if err != nil {
		return nil, err
	}
	return ast.CreateIndex{Name: name, Table: table, Columns: columns, Unique: unique}, nil
}

func (parser *Parser) parseDrop() (ast.Statement, error) {
	if err := parser.expectKeyword("table"); err != nil {
		return nil, err
	}
	ifExists := false
	if parser.matchKeyword("if") {
		if err := parser.expectKeyword("exists"); err != nil {
			return nil, err
		}
		ifExists = true
	}
	name, err := parser.parseName()
	return ast.DropTable{Name: name, IfExists: ifExists}, err
}

func (parser *Parser) parseInsert() (ast.Statement, error) {
	if err := parser.expectKeyword("into"); err != nil {
		return nil, err
	}
	table, err := parser.parseName()
	if err != nil {
		return nil, err
	}
	statement := ast.Insert{Table: table}
	if parser.peek().Kind == lexer.LeftParen {
		statement.Columns, err = parser.parseIdentifierList()
		if err != nil {
			return nil, err
		}
	}
	if err := parser.expectKeyword("values"); err != nil {
		return nil, err
	}
	for {
		if err := parser.expect(lexer.LeftParen, "("); err != nil {
			return nil, err
		}
		var row []ast.Expression
		for {
			expression, err := parser.parseExpression(1)
			if err != nil {
				return nil, err
			}
			row = append(row, expression)
			if parser.match(lexer.RightParen) {
				break
			}
			if err := parser.expect(lexer.Comma, ","); err != nil {
				return nil, err
			}
		}
		statement.Rows = append(statement.Rows, row)
		if !parser.match(lexer.Comma) {
			break
		}
	}
	return statement, nil
}

func (parser *Parser) parseSelect() (ast.Statement, error) {
	statement := ast.Select{}
	for {
		expression, err := parser.parseExpression(1)
		if err != nil {
			return nil, err
		}
		item := ast.SelectItem{Expression: expression}
		if parser.matchKeyword("as") {
			item.Alias, err = parser.expectIdentifier("column alias")
			if err != nil {
				return nil, err
			}
		}
		statement.Items = append(statement.Items, item)
		if !parser.match(lexer.Comma) {
			break
		}
	}
	if err := parser.expectKeyword("from"); err != nil {
		return nil, err
	}
	var err error
	statement.From, err = parser.parseName()
	if err != nil {
		return nil, err
	}
	if parser.matchKeyword("where") {
		statement.Where, err = parser.parseExpression(1)
		if err != nil {
			return nil, err
		}
	}
	if parser.matchKeyword("order") {
		if err := parser.expectKeyword("by"); err != nil {
			return nil, err
		}
		for {
			expression, err := parser.parseExpression(1)
			if err != nil {
				return nil, err
			}
			order := ast.OrderBy{Expression: expression}
			if parser.matchKeyword("desc") {
				order.Descending = true
			} else {
				_ = parser.matchKeyword("asc")
			}
			statement.OrderBy = append(statement.OrderBy, order)
			if !parser.match(lexer.Comma) {
				break
			}
		}
	}
	if parser.matchKeyword("limit") {
		token, err := parser.expectKind(lexer.Number, "LIMIT value")
		if err != nil {
			return nil, err
		}
		if strings.Contains(token.Literal, ".") {
			return nil, parser.errorf("LIMIT must be an integer")
		}
		limit, err := strconv.ParseInt(token.Literal, 10, 64)
		if err != nil {
			return nil, parser.errorf("invalid LIMIT value")
		}
		statement.Limit = &limit
	}
	return statement, nil
}

func (parser *Parser) parseUpdate() (ast.Statement, error) {
	table, err := parser.parseName()
	if err != nil {
		return nil, err
	}
	if err := parser.expectKeyword("set"); err != nil {
		return nil, err
	}
	statement := ast.Update{Table: table}
	for {
		column, err := parser.expectIdentifier("assigned column")
		if err != nil {
			return nil, err
		}
		if err := parser.expect(lexer.Equal, "="); err != nil {
			return nil, err
		}
		value, err := parser.parseExpression(1)
		if err != nil {
			return nil, err
		}
		statement.Assignments = append(statement.Assignments, ast.Assignment{Column: column, Value: value})
		if !parser.match(lexer.Comma) {
			break
		}
	}
	if parser.matchKeyword("where") {
		statement.Where, err = parser.parseExpression(1)
	}
	return statement, err
}

func (parser *Parser) parseDelete() (ast.Statement, error) {
	if err := parser.expectKeyword("from"); err != nil {
		return nil, err
	}
	table, err := parser.parseName()
	if err != nil {
		return nil, err
	}
	statement := ast.Delete{Table: table}
	if parser.matchKeyword("where") {
		statement.Where, err = parser.parseExpression(1)
	}
	return statement, err
}

func (parser *Parser) parseExpression(minimumPrecedence int) (ast.Expression, error) {
	left, err := parser.parsePrefixExpression()
	if err != nil {
		return nil, err
	}
	for {
		operator, precedence := parser.infixOperator()
		if precedence < minimumPrecedence {
			break
		}
		parser.index++
		right, err := parser.parseExpression(precedence + 1)
		if err != nil {
			return nil, err
		}
		left = ast.BinaryExpression{Left: left, Operator: operator, Right: right}
	}
	return left, nil
}

func (parser *Parser) parsePrefixExpression() (ast.Expression, error) {
	token := parser.peek()
	if token.Kind == lexer.Plus || token.Kind == lexer.Minus || parser.isKeyword(token, "not") {
		parser.index++
		value, err := parser.parseExpression(6)
		if err != nil {
			return nil, err
		}
		return ast.UnaryExpression{Operator: strings.ToUpper(token.Literal), Value: value}, nil
	}
	switch token.Kind {
	case lexer.Number:
		parser.index++
		return ast.Literal{Kind: ast.NumberLiteral, Raw: token.Literal}, nil
	case lexer.String:
		parser.index++
		return ast.Literal{Kind: ast.StringLiteral, Raw: token.Literal}, nil
	case lexer.Star:
		parser.index++
		return ast.Star{}, nil
	case lexer.LeftParen:
		parser.index++
		expression, err := parser.parseExpression(1)
		if err != nil {
			return nil, err
		}
		if err := parser.expect(lexer.RightParen, ")"); err != nil {
			return nil, err
		}
		return expression, nil
	case lexer.Identifier:
		if parser.isKeyword(token, "null") {
			parser.index++
			return ast.Literal{Kind: ast.NullLiteral, Raw: "NULL"}, nil
		}
		if parser.isKeyword(token, "true") || parser.isKeyword(token, "false") {
			parser.index++
			return ast.Literal{Kind: ast.BoolLiteral, Raw: token.Literal}, nil
		}
		name, err := parser.parseName()
		if err != nil {
			return nil, err
		}
		if parser.match(lexer.LeftParen) {
			if len(name.Parts) != 1 {
				return nil, parser.errorf("function name cannot be qualified")
			}
			call := ast.FunctionCall{Name: name.Parts[0]}
			if !parser.match(lexer.RightParen) {
				for {
					argument, err := parser.parseExpression(1)
					if err != nil {
						return nil, err
					}
					call.Arguments = append(call.Arguments, argument)
					if parser.match(lexer.RightParen) {
						break
					}
					if err := parser.expect(lexer.Comma, ","); err != nil {
						return nil, err
					}
				}
			}
			return call, nil
		}
		return ast.ColumnReference{Name: name}, nil
	default:
		return nil, parser.errorf("expected expression")
	}
}

func (parser *Parser) infixOperator() (string, int) {
	token := parser.peek()
	if parser.isKeyword(token, "or") {
		return "OR", 1
	}
	if parser.isKeyword(token, "and") {
		return "AND", 2
	}
	switch token.Kind {
	case lexer.Equal, lexer.NotEqual, lexer.Less, lexer.LessEqual, lexer.Greater, lexer.GreaterEqual:
		return token.Literal, 3
	case lexer.Plus, lexer.Minus:
		return token.Literal, 4
	case lexer.Star, lexer.Slash, lexer.Percent:
		return token.Literal, 5
	default:
		return "", 0
	}
}

func (parser *Parser) parseIdentifierList() ([]string, error) {
	if err := parser.expect(lexer.LeftParen, "("); err != nil {
		return nil, err
	}
	var result []string
	for {
		identifier, err := parser.expectIdentifier("identifier")
		if err != nil {
			return nil, err
		}
		result = append(result, identifier)
		if parser.match(lexer.RightParen) {
			return result, nil
		}
		if err := parser.expect(lexer.Comma, ","); err != nil {
			return nil, err
		}
	}
}

func (parser *Parser) parseName() (ast.Name, error) {
	first, err := parser.expectIdentifier("name")
	if err != nil {
		return ast.Name{}, err
	}
	name := ast.Name{Parts: []string{first}}
	for parser.match(lexer.Dot) {
		part, err := parser.expectIdentifier("name after dot")
		if err != nil {
			return ast.Name{}, err
		}
		name.Parts = append(name.Parts, part)
		if len(name.Parts) > 3 {
			return ast.Name{}, parser.errorf("names may have at most three parts")
		}
	}
	return name, nil
}

func (parser *Parser) expectIdentifier(label string) (string, error) {
	token, err := parser.expectKind(lexer.Identifier, label)
	return token.Literal, err
}

func (parser *Parser) expectKeyword(keyword string) error {
	if !parser.matchKeyword(keyword) {
		return parser.errorf("expected %s", strings.ToUpper(keyword))
	}
	return nil
}

func (parser *Parser) expect(kind lexer.Kind, label string) error {
	_, err := parser.expectKind(kind, label)
	return err
}

func (parser *Parser) expectKind(kind lexer.Kind, label string) (lexer.Token, error) {
	if parser.peek().Kind != kind {
		return lexer.Token{}, parser.errorf("expected %s", label)
	}
	token := parser.peek()
	parser.index++
	return token, nil
}

func (parser *Parser) match(kind lexer.Kind) bool {
	if parser.peek().Kind != kind {
		return false
	}
	parser.index++
	return true
}

func (parser *Parser) matchKeyword(keyword string) bool {
	if !parser.isKeyword(parser.peek(), keyword) {
		return false
	}
	parser.index++
	return true
}

func (parser *Parser) isKeyword(token lexer.Token, keyword string) bool {
	return token.Kind == lexer.Identifier && !token.Quoted && token.Literal == keyword
}

func (parser *Parser) peek() lexer.Token {
	if parser.index >= len(parser.tokens) {
		return parser.tokens[len(parser.tokens)-1]
	}
	return parser.tokens[parser.index]
}

func (parser *Parser) errorf(format string, arguments ...any) error {
	position := parser.peek().Position
	return fmt.Errorf("sql parser at %d:%d: %s", position.Line, position.Column, fmt.Sprintf(format, arguments...))
}
