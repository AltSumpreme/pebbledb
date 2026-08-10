package parser_test

import (
	"testing"

	"pebbledb/sql/ast"
	"pebbledb/sql/parser"
)

func TestParseCreateTable(t *testing.T) {
	statement, err := parser.Parse(`CREATE TABLE public.users (
        id BIGINT PRIMARY KEY,
        name TEXT NOT NULL,
        email TEXT UNIQUE,
        balance DECIMAL(12,2) NULL
    );`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	create, ok := statement.(ast.CreateTable)
	if !ok {
		t.Fatalf("got %T", statement)
	}
	if create.Name.String() != "public.users" || len(create.Columns) != 4 || len(create.PrimaryKey) != 1 || create.PrimaryKey[0] != "id" {
		t.Fatalf("unexpected CREATE TABLE: %+v", create)
	}
	if create.Columns[1].Nullable || !create.Columns[2].Unique || create.Columns[3].TypeName != "decimal(12,2)" {
		t.Fatalf("unexpected column definitions: %+v", create.Columns)
	}
}

func TestParseInsertSelectUpdateDelete(t *testing.T) {
	statements, err := parser.ParseStatements(`
        INSERT INTO users (id, name, age) VALUES (1, 'Alice', 21), (2, 'Bob', 19);
        SELECT name, age + 1 AS next_age, COUNT(*) AS total
          FROM public.users
         WHERE age >= 18 AND name <> 'X'
         ORDER BY name DESC, age ASC LIMIT 10;
        UPDATE users SET age = age + 1, name = 'Updated' WHERE id = 1;
        DELETE FROM users WHERE id = 2;
    `)
	if err != nil {
		t.Fatalf("parse statements: %v", err)
	}
	if len(statements) != 4 {
		t.Fatalf("got %d statements", len(statements))
	}
	insert := statements[0].(ast.Insert)
	if len(insert.Columns) != 3 || len(insert.Rows) != 2 {
		t.Fatalf("unexpected INSERT: %+v", insert)
	}
	selectStatement := statements[1].(ast.Select)
	if len(selectStatement.Items) != 3 || len(selectStatement.OrderBy) != 2 || selectStatement.Limit == nil || *selectStatement.Limit != 10 {
		t.Fatalf("unexpected SELECT: %+v", selectStatement)
	}
	filter, ok := selectStatement.Where.(ast.BinaryExpression)
	if !ok || filter.Operator != "AND" {
		t.Fatalf("expression precedence did not produce AND root: %#v", selectStatement.Where)
	}
	update := statements[2].(ast.Update)
	if len(update.Assignments) != 2 || update.Where == nil {
		t.Fatalf("unexpected UPDATE: %+v", update)
	}
	if deleteStatement := statements[3].(ast.Delete); deleteStatement.Where == nil {
		t.Fatalf("unexpected DELETE: %+v", deleteStatement)
	}
}

func TestParseCatalogAndTransactionStatements(t *testing.T) {
	statements, err := parser.ParseStatements(`
        CREATE DATABASE shop;
        CREATE SCHEMA shop.sales;
        CREATE UNIQUE INDEX users_email_idx ON sales.users (email);
        DROP TABLE IF EXISTS missing;
        SHOW TABLES; DESCRIBE users; BEGIN; COMMIT; ROLLBACK;
    `)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(statements) != 9 {
		t.Fatalf("got %d statements, want 9", len(statements))
	}
	index := statements[2].(ast.CreateIndex)
	if !index.Unique || index.Table.String() != "sales.users" || len(index.Columns) != 1 {
		t.Fatalf("unexpected index: %+v", index)
	}
	drop := statements[3].(ast.DropTable)
	if !drop.IfExists {
		t.Fatal("IF EXISTS was not preserved")
	}
}

func TestParserRejectsInvalidSQL(t *testing.T) {
	for _, input := range []string{
		"CREATE TABLE empty ()",
		"INSERT users VALUES (1)",
		"SELECT FROM users",
		"SELECT * FROM users LIMIT 1.5",
		"DELETE users",
	} {
		if _, err := parser.Parse(input); err == nil {
			t.Fatalf("expected %q to fail", input)
		}
	}
}
