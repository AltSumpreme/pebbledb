package parser

import (
	"errors"
	"fmt"
	"pebbledb/db"
	"strings"
	"unicode"
)

type CommandType string

const (
	CommandTypeCreate CommandType = "CREATE"
	CommandTypeInsert CommandType = "INSERT"
	CommandTypeSelect CommandType = "SELECT"
	CommandTypeDelete CommandType = "DELETE"
	CommandTypeDrop   CommandType = "DROP"
)

type Command struct {
	Type       CommandType
	Tablename  string
	Columns    []db.Column
	Values     []string
	AllColumns bool
}

func Parse(input string) (*Command, error) {
	tokens, err := tokenize(input)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return nil, errors.New("empty command")
	}

	p := &parser{tokens: tokens}
	switch strings.ToUpper(p.peek()) {
	case "CREATE":
		return p.parseCreate()
	case "INSERT":
		return p.parseInsert()
	case "SELECT":
		return p.parseSelect()
	case "DELETE":
		return p.parseDelete()
	case "DROP":
		return p.parseDrop()
	case "EXIT":
		return nil, nil
	default:
		return nil, errors.New("unknown command")
	}
}

type parser struct {
	tokens []string
	pos    int
}

func (p *parser) parseCreate() (*Command, error) {
	p.expectKeyword("CREATE")
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	tableName, err := p.expectIdentifier("table name")
	if err != nil {
		return nil, err
	}

	var cols []db.Column
	for !p.done() {
		colName, err := p.expectIdentifier("column name")
		if err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		colTypeRaw, err := p.expectIdentifier("column type")
		if err != nil {
			return nil, err
		}
		colType, err := parseColumnType(colTypeRaw)
		if err != nil {
			return nil, err
		}
		cols = append(cols, db.Column{Name: colName, Type: colType})
		if p.peek() == "," {
			p.next()
		}
	}
	if len(cols) == 0 {
		return nil, errors.New("CREATE requires at least one column")
	}
	return &Command{Type: CommandTypeCreate, Tablename: tableName, Columns: cols}, nil
}

func (p *parser) parseInsert() (*Command, error) {
	p.expectKeyword("INSERT")
	if err := p.expectKeyword("TO"); err != nil {
		return nil, err
	}
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	tableName, err := p.expectIdentifier("table name")
	if err != nil {
		return nil, err
	}
	cols, err := p.parseIdentifierList()
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(p.peek(), "FROM") {
		p.next()
	}
	if err := p.expectKeyword("VALUES"); err != nil {
		return nil, err
	}
	values, err := p.parseValueList()
	if err != nil {
		return nil, err
	}
	if !p.done() {
		return nil, fmt.Errorf("unexpected token %q", p.peek())
	}
	if len(cols) != len(values) {
		return nil, errors.New("number of columns and values do not match")
	}

	return &Command{
		Type:      CommandTypeInsert,
		Tablename: tableName,
		Columns:   cols,
		Values:    values,
	}, nil
}

func (p *parser) parseSelect() (*Command, error) {
	p.expectKeyword("SELECT")
	var cols []db.Column
	allColumns := false
	if p.peek() == "*" {
		allColumns = true
		p.next()
	} else {
		for {
			colName, err := p.expectIdentifier("column name")
			if err != nil {
				return nil, err
			}
			cols = append(cols, db.Column{Name: colName})
			if p.peek() != "," {
				break
			}
			p.next()
		}
	}
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	tableName, err := p.expectIdentifier("table name")
	if err != nil {
		return nil, err
	}
	if !p.done() {
		return nil, fmt.Errorf("unexpected token %q", p.peek())
	}
	return &Command{
		Type:       CommandTypeSelect,
		Tablename:  tableName,
		Columns:    cols,
		AllColumns: allColumns,
	}, nil
}

func (p *parser) parseDelete() (*Command, error) {
	p.expectKeyword("DELETE")
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	tableName, err := p.expectIdentifier("table name")
	if err != nil {
		return nil, err
	}
	if !p.done() {
		return nil, fmt.Errorf("unexpected token %q", p.peek())
	}
	return &Command{Type: CommandTypeDelete, Tablename: tableName}, nil
}

func (p *parser) parseDrop() (*Command, error) {
	p.expectKeyword("DROP")
	if err := p.expectKeyword("TABLE"); err != nil {
		return nil, err
	}
	tableName, err := p.expectIdentifier("table name")
	if err != nil {
		return nil, err
	}
	if !p.done() {
		return nil, fmt.Errorf("unexpected token %q", p.peek())
	}
	return &Command{Type: CommandTypeDrop, Tablename: tableName}, nil
}

func (p *parser) parseIdentifierList() ([]db.Column, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var cols []db.Column
	for {
		colName, err := p.expectIdentifier("column name")
		if err != nil {
			return nil, err
		}
		cols = append(cols, db.Column{Name: colName})
		if p.peek() == ")" {
			p.next()
			break
		}
		if err := p.expect(","); err != nil {
			return nil, err
		}
	}
	return cols, nil
}

func (p *parser) parseValueList() ([]string, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var values []string
	for {
		if p.done() {
			return nil, errors.New("unexpected end of input in value list")
		}
		values = append(values, p.next())
		if p.peek() == ")" {
			p.next()
			break
		}
		if err := p.expect(","); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func (p *parser) expectKeyword(keyword string) error {
	if !strings.EqualFold(p.peek(), keyword) {
		return fmt.Errorf("expected %s", keyword)
	}
	p.next()
	return nil
}

func (p *parser) expectIdentifier(label string) (string, error) {
	if p.done() {
		return "", fmt.Errorf("expected %s", label)
	}
	token := p.next()
	if isPunctuation(token) {
		return "", fmt.Errorf("expected %s", label)
	}
	return token, nil
}

func (p *parser) expect(token string) error {
	if p.peek() != token {
		return fmt.Errorf("expected %s", token)
	}
	p.next()
	return nil
}

func (p *parser) peek() string {
	if p.done() {
		return ""
	}
	return p.tokens[p.pos]
}

func (p *parser) next() string {
	token := p.peek()
	if !p.done() {
		p.pos++
	}
	return token
}

func (p *parser) done() bool {
	return p.pos >= len(p.tokens)
}

func parseColumnType(raw string) (db.FieldType, error) {
	switch strings.ToUpper(raw) {
	case "INT":
		return db.TypeInt, nil
	case "STRING":
		return db.TypeString, nil
	default:
		return "", errors.New("unsupported column type: " + raw)
	}
}

func tokenize(input string) ([]string, error) {
	var tokens []string
	for i := 0; i < len(input); {
		r := rune(input[i])
		if unicode.IsSpace(r) {
			i++
			continue
		}
		if strings.ContainsRune("(),:*", r) {
			tokens = append(tokens, string(r))
			i++
			continue
		}
		if r == '\'' || r == '"' {
			value, next, err := readQuoted(input, i, byte(r))
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, value)
			i = next
			continue
		}

		start := i
		for i < len(input) {
			r = rune(input[i])
			if unicode.IsSpace(r) || strings.ContainsRune("(),:*'\"", r) {
				break
			}
			i++
		}
		tokens = append(tokens, input[start:i])
	}
	return tokens, nil
}

func readQuoted(input string, start int, quote byte) (string, int, error) {
	var builder strings.Builder
	for i := start + 1; i < len(input); i++ {
		if input[i] == '\\' && i+1 < len(input) {
			i++
			builder.WriteByte(input[i])
			continue
		}
		if input[i] == quote {
			return builder.String(), i + 1, nil
		}
		builder.WriteByte(input[i])
	}
	return "", 0, errors.New("unterminated quoted value")
}

func isPunctuation(token string) bool {
	return len(token) == 1 && strings.Contains("(),:*", token)
}
