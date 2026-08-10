package binder_test

import (
	"errors"
	"testing"

	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/sql/binder"
	"pebbledb/sql/parser"
	"pebbledb/storage/lsm"
	"pebbledb/types"
)

func TestBindCreateTableAndTypedInsert(t *testing.T) {
	binderValue, catalogValue, _, schema := openTestBinder(t)
	bound := bindSQL(t, binderValue, `CREATE TABLE users (
        id BIGINT PRIMARY KEY,
        name TEXT NOT NULL,
        email TEXT NULL,
        balance DECIMAL(10,2) NOT NULL
    )`).(binder.CreateTable)
	if bound.SchemaID != schema.ID || len(bound.Columns) != 4 || len(bound.PrimaryKey) != 1 || bound.Columns[0].Nullable {
		t.Fatalf("unexpected bound CREATE: %+v", bound)
	}
	table, err := catalogValue.CreateTable(bound.SchemaID, bound.Name, bound.Columns, bound.PrimaryKey)
	if err != nil {
		t.Fatalf("install table: %v", err)
	}

	insert := bindSQL(t, binderValue, `INSERT INTO users (balance, name, id) VALUES
        (99.95, 'Alice', 1), (-1.25, 'Bob', 2)`).(binder.Insert)
	if insert.Table.ID != table.ID || len(insert.Rows) != 2 {
		t.Fatalf("unexpected bound INSERT: %+v", insert)
	}
	if insert.Rows[0].Values[3].String() != "NULL" || insert.Rows[1].Values[4].String() != "-1.25" {
		t.Fatalf("unexpected typed rows: %+v", insert.Rows)
	}
	if _, err := bindSQLResult(binderValue, `INSERT INTO users (id, balance) VALUES (3, 1.00)`); err == nil {
		t.Fatal("expected missing non-null name to fail")
	}
	if _, err := bindSQLResult(binderValue, `INSERT INTO users VALUES (NULL, 'X', NULL, 1.00)`); err == nil {
		t.Fatal("expected NULL primary key to fail")
	}
}

func TestBindSelectResolvesAndTypesExpressions(t *testing.T) {
	binderValue, catalogValue, _, schema := openTestBinder(t)
	table, err := catalogValue.CreateTable(schema.ID, "users", []codec.ColumnDescriptor{
		{ID: 1, Name: "id", Type: types.BigIntType()},
		{ID: 2, Name: "name", Type: types.TextType()},
		{ID: 3, Name: "age", Type: types.IntType(), Nullable: true},
		{ID: 4, Name: "active", Type: types.BoolType()},
	}, []uint32{1})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	selectStatement := bindSQL(t, binderValue, `SELECT *, age + 1 AS next_age, lower(name)
        FROM users WHERE active = true AND age >= 18 ORDER BY name DESC LIMIT 5`).(binder.Select)
	if selectStatement.Table.ID != table.ID || len(selectStatement.Projection) != 6 || selectStatement.Filter.Type != types.BoolType() || len(selectStatement.Ordering) != 1 {
		t.Fatalf("unexpected bound SELECT: %+v", selectStatement)
	}
	if selectStatement.Projection[4].Expression.Type != types.IntType() || selectStatement.Projection[5].Expression.Type != types.TextType() {
		t.Fatalf("unexpected projection types: %+v", selectStatement.Projection)
	}
	if _, err := bindSQLResult(binderValue, "SELECT missing FROM users"); err == nil {
		t.Fatal("expected unknown column to fail")
	}
	if _, err := bindSQLResult(binderValue, "SELECT id FROM users WHERE name"); err == nil {
		t.Fatal("expected non-BOOL WHERE to fail")
	}
}

func TestBindContextuallyCoercesDecimalLiterals(t *testing.T) {
	binderValue, catalogValue, _, schema := openTestBinder(t)
	decimalType, _ := types.DecimalType(10, 2)
	_, err := catalogValue.CreateTable(schema.ID, "products", []codec.ColumnDescriptor{
		{ID: 1, Name: "id", Type: types.BigIntType()},
		{ID: 2, Name: "price", Type: decimalType},
	}, []uint32{1})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	selectStatement := bindSQL(t, binderValue, "SELECT price FROM products WHERE price >= 1.00").(binder.Select)
	if selectStatement.Filter.Arguments[1].Type != decimalType || selectStatement.Filter.Arguments[1].Literal.String() != "1.00" {
		t.Fatalf("comparison literal was not coerced: %+v", selectStatement.Filter)
	}
	update := bindSQL(t, binderValue, "UPDATE products SET price = -2.50 WHERE id = 1").(binder.Update)
	if update.Assignments[0].Value.Type != decimalType || update.Assignments[0].Value.Literal.String() != "-2.50" {
		t.Fatalf("assignment literal was not coerced: %+v", update.Assignments[0])
	}
}

func TestBindQualifiedNamesIndexAndIfExists(t *testing.T) {
	binderValue, catalogValue, database, schema := openTestBinder(t)
	_, err := catalogValue.CreateTable(schema.ID, "users", []codec.ColumnDescriptor{
		{ID: 1, Name: "id", Type: types.BigIntType()},
		{ID: 2, Name: "email", Type: types.TextType()},
	}, []uint32{1})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	selectStatement := bindSQL(t, binderValue, "SELECT users.email FROM pebbledb.public.users").(binder.Select)
	if selectStatement.Table.SchemaID != schema.ID || database.Name != "pebbledb" {
		t.Fatalf("qualified resolution failed: %+v", selectStatement)
	}
	index := bindSQL(t, binderValue, "CREATE UNIQUE INDEX users_email_idx ON public.users(email)").(binder.CreateIndex)
	if !index.Unique || len(index.ColumnIDs) != 1 || index.ColumnIDs[0] != 2 {
		t.Fatalf("unexpected index binding: %+v", index)
	}
	drop := bindSQL(t, binderValue, "DROP TABLE IF EXISTS missing").(binder.DropTable)
	if !drop.IfExists || drop.Missing != "missing" {
		t.Fatalf("unexpected IF EXISTS binding: %+v", drop)
	}
	if _, err := bindSQLResult(binderValue, "DROP TABLE missing"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("missing DROP error = %v", err)
	}
}

func TestBindJoinTracksTableColumnIdentity(t *testing.T) {
	binderValue, catalogValue, _, schema := openTestBinder(t)
	users, err := catalogValue.CreateTable(schema.ID, "users", []codec.ColumnDescriptor{
		{ID: 1, Name: "id", Type: types.BigIntType()},
		{ID: 2, Name: "name", Type: types.TextType()},
	}, []uint32{1})
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	orders, err := catalogValue.CreateTable(schema.ID, "orders", []codec.ColumnDescriptor{
		{ID: 1, Name: "id", Type: types.BigIntType()},
		{ID: 2, Name: "user_id", Type: types.BigIntType()},
	}, []uint32{1})
	if err != nil {
		t.Fatalf("orders: %v", err)
	}
	selectStatement := bindSQL(t, binderValue, `SELECT users.name, orders.id
        FROM users JOIN orders ON users.id = orders.user_id`).(binder.Select)
	if len(selectStatement.Joins) != 1 || selectStatement.Joins[0].Table.ID != orders.ID {
		t.Fatalf("unexpected join: %+v", selectStatement.Joins)
	}
	if selectStatement.Projection[0].Expression.TableID != users.ID || selectStatement.Projection[1].Expression.TableID != orders.ID {
		t.Fatalf("bound columns lost table identity: %+v", selectStatement.Projection)
	}
	if _, err := bindSQLResult(binderValue, "SELECT id FROM users JOIN orders ON users.id = orders.user_id"); err == nil {
		t.Fatal("expected unqualified ambiguous id to fail")
	}
}

func openTestBinder(t *testing.T) (*binder.Binder, *catalog.Catalog, catalog.DatabaseDescriptor, catalog.SchemaDescriptor) {
	t.Helper()
	store, err := lsm.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open LSM: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalogValue, _ := catalog.New(store)
	database, schema, err := catalogValue.EnsureDefaults()
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	binderValue, err := binder.New(catalogValue, binder.Context{DatabaseID: database.ID, SchemaID: schema.ID})
	if err != nil {
		t.Fatalf("new binder: %v", err)
	}
	return binderValue, catalogValue, database, schema
}

func bindSQL(t *testing.T, binderValue *binder.Binder, sql string) binder.Statement {
	t.Helper()
	statement, err := bindSQLResult(binderValue, sql)
	if err != nil {
		t.Fatalf("bind %q: %v", sql, err)
	}
	return statement
}

func bindSQLResult(binderValue *binder.Binder, sql string) (binder.Statement, error) {
	statement, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	return binderValue.Bind(statement)
}
