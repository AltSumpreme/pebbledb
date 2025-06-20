package parser

import (
	"errors"
	"pebbledb/db"
	"strings"
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

	tokens := strings.Fields(input)
	if len(tokens) == 0 {
		return nil, errors.New("empty command")
	}
	command := strings.ToUpper(tokens[0])
	switch command {
	case "CREATE":
		// e.g., CREATE TABLE table_name id:int name:string
		if len(tokens) < 4 {
			return nil, errors.New("invalid CREATE command")
		}
		var cols []db.Column
		for _, col := range tokens[3:] {
			parts := strings.Split(col, ":")
			if len(parts) != 2 {
				return nil, errors.New("invalid column definition")
			}
			colName := parts[0]
			colTypeRaw := strings.ToUpper(parts[1])
			var colType db.FieldType
			switch colTypeRaw {
			case "INT":
				colType = db.TypeInt
			case "STRING":
				colType = db.TypeString
			default:
				return nil, errors.New("unsupported column type: " + colTypeRaw)
			}
			cols = append(cols, db.Column{
				Name: colName,
				Type: colType,
			})
		}
		return &Command{
			Type:      CommandTypeCreate,
			Tablename: tokens[2],
			Columns:   cols,
		}, nil
	case "INSERT":
		// e.g., INSERT TO TABLE table_name (col1, col2) VALUES (val1, val2)
		if len(tokens) < 7 || tokens[1] != "TO" || tokens[2] != "TABLE" || tokens[5] != "VALUES" {
			return nil, errors.New("invalid INSERT command")
		}
		tableName := tokens[3]
		colToken := strings.Trim(tokens[4], "()")
		colParts := strings.Split(colToken, ",")
		var vals []string
		if len(tokens) > 6 {
			valsToken := strings.Trim(tokens[6], "()")
			vals = strings.Split(valsToken, ",")
		}
		if len(colParts) != len(vals) {
			return nil, errors.New("number of columns and values do not match")
		}

		var cols []db.Column
		for _, col := range colParts {
			cols = append(cols, db.Column{
				Name: col,
				Type: db.TypeString,
			})
		}

		return &Command{
			Type:      CommandTypeInsert,
			Tablename: tableName,
			Columns:   cols,
			Values:    vals,
		}, nil

	case "SELECT":
		// e.g., SELECT col1, col2 FROM table_name
		if len(tokens) < 4 || tokens[2] != "FROM" {
			return nil, errors.New("invalid SELECT command")
		}
		var cols []db.Column
		var allcols bool
		if tokens[1] == "*" {
			allcols = true
		} else {
			for _, col := range strings.Split(tokens[1], ",") {
				cols = append(cols, db.Column{
					Name: col,
				})
			}
		}
		return &Command{
			Type:       CommandTypeSelect,
			Tablename:  tokens[3],
			Columns:    cols,
			AllColumns: allcols,
		}, nil
	case "DELETE":
		// e.g., DELETE FROM table_name WHERE condition
		if len(tokens) < 4 || tokens[1] != "FROM" {
			return nil, errors.New("invalid DELETE command")
		}
		return &Command{
			Type:      CommandTypeDelete,
			Tablename: tokens[2],
		}, nil
	case "DROP":
		// e.g., DROP TABLE table_name
		if len(tokens) < 3 || tokens[1] != "TABLE" {
			return nil, errors.New("invalid DROP command")
		}
		return &Command{
			Type:      CommandTypeDrop,
			Tablename: tokens[2],
		}, nil
	default:
		// Handle other commands or return an error
		if command == "EXIT" {
			return nil, nil
		}
	}

	return nil, errors.New("unknown command")
}
