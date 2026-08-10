package engine_test

import (
	"strings"
	"testing"

	"pebbledb/sql/engine"
)

func TestEndToEndSQLAndRestart(t *testing.T) {
	directory := t.TempDir()
	database, err := engine.Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	results, err := database.Execute(`
        CREATE TABLE users (
            id BIGINT PRIMARY KEY,
            name TEXT NOT NULL,
            age INT NULL,
            balance DECIMAL(10,2) NOT NULL
        );
        INSERT INTO users (id, name, age, balance) VALUES
            (3, 'Chandra', 27, 120.00),
            (1, 'Alice', 21, 99.95),
            (2, 'Bob', NULL, -5.00);
        SELECT id, upper(name) AS name, age
          FROM users
         WHERE age >= 21
         ORDER BY id DESC;
    `)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(results) != 3 || results[1].RowsAffected != 3 {
		t.Fatalf("unexpected results: %+v", results)
	}
	selected := results[2]
	if len(selected.Rows) != 2 || selected.Rows[0][0].String() != "3" || selected.Rows[0][1].String() != "CHANDRA" || selected.Rows[1][0].String() != "1" {
		t.Fatalf("unexpected SELECT rows: %+v", selected.Rows)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	database, err = engine.Open(directory)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	results, err = database.Execute("SELECT * FROM users ORDER BY id")
	if err != nil {
		t.Fatalf("select after restart: %v", err)
	}
	if len(results[0].Rows) != 3 || results[0].Rows[1][1].String() != "Bob" || results[0].Rows[1][2].String() != "NULL" {
		t.Fatalf("unexpected persisted rows: %+v", results[0].Rows)
	}
}

func TestUpdateDeleteAggregateAndCatalogQueries(t *testing.T) {
	database, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	results, err := database.Execute(`
        CREATE TABLE products (id BIGINT PRIMARY KEY, name TEXT NOT NULL, price DECIMAL(10,2) NOT NULL);
        INSERT INTO products VALUES (1, 'Keyboard', 99.99), (2, 'Mouse', 25.00), (3, 'Desk', 200.00);
        UPDATE products SET price = price + 5.00 WHERE id = 2;
        DELETE FROM products WHERE id = 1;
        SELECT COUNT(*) AS count, SUM(price) AS total FROM products;
        SHOW TABLES;
        DESCRIBE products;
    `)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if results[2].RowsAffected != 1 || results[3].RowsAffected != 1 {
		t.Fatalf("unexpected update/delete counts: %+v", results)
	}
	aggregate := results[4]
	if len(aggregate.Rows) != 1 || aggregate.Rows[0][0].String() != "2" || aggregate.Rows[0][1].String() != "230.00" {
		t.Fatalf("unexpected aggregate: %+v", aggregate.Rows)
	}
	if results[5].Rows[0][0].String() != "products" || len(results[6].Rows) != 3 {
		t.Fatalf("unexpected catalog results: show=%+v describe=%+v", results[5].Rows, results[6].Rows)
	}
}

func TestExplainAndExecutionErrors(t *testing.T) {
	database, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if _, err := database.Execute("CREATE TABLE users (id BIGINT PRIMARY KEY, name TEXT NOT NULL)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	explanation, err := database.Explain("SELECT name FROM users WHERE id = 1 LIMIT 1")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	for _, expected := range []string{"Limit", "Project", "Filter", "primary-key-lookup"} {
		if !strings.Contains(explanation, expected) {
			t.Fatalf("explanation lacks %q:\n%s", expected, explanation)
		}
	}
	if _, err := database.Execute("INSERT INTO users VALUES (1, 'A'), (1, 'B')"); err == nil {
		t.Fatal("expected duplicate key batch to fail")
	}
	results, err := database.Execute("SELECT COUNT(*) FROM users")
	if err != nil || results[0].Rows[0][0].String() != "0" {
		t.Fatalf("failed duplicate batch inserted rows: results=%+v err=%v", results, err)
	}
	results, err = database.Execute("BEGIN; INSERT INTO users VALUES (2, 'B'); ROLLBACK; SELECT COUNT(*) FROM users")
	if err != nil {
		t.Fatalf("rollback transaction: %v", err)
	}
	if results[0].Message != "BEGIN" || results[2].Message != "ROLLBACK" || results[3].Rows[0][0].String() != "0" {
		t.Fatalf("unexpected rollback results: %+v", results)
	}
}

func TestExplicitTransactionsCommitRollbackAndAbort(t *testing.T) {
	directory := t.TempDir()
	database, err := engine.Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := database.Execute("CREATE TABLE accounts (id BIGINT PRIMARY KEY, balance INT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	results, err := database.Execute("BEGIN; INSERT INTO accounts VALUES (1, 100); SELECT balance FROM accounts WHERE id = 1; COMMIT")
	if err != nil {
		t.Fatalf("commit transaction: %v", err)
	}
	if len(results) != 4 || results[2].Rows[0][0].String() != "100" || results[3].Message != "COMMIT" {
		t.Fatalf("unexpected commit results: %+v", results)
	}
	if _, err := database.Execute("BEGIN; UPDATE accounts SET balance = 50 WHERE id = 1; ROLLBACK"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	results, err = database.Execute("SELECT balance FROM accounts WHERE id = 1")
	if err != nil || results[0].Rows[0][0].String() != "100" {
		t.Fatalf("rollback leaked update: %+v, %v", results, err)
	}

	if _, err := database.Execute("BEGIN; INSERT INTO accounts VALUES (1, 200)"); err == nil {
		t.Fatal("expected duplicate key to abort transaction")
	}
	if _, err := database.Execute("SELECT * FROM accounts"); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected aborted transaction error, got %v", err)
	}
	if results, err := database.Execute("ROLLBACK"); err != nil || results[0].Message != "ROLLBACK" {
		t.Fatalf("rollback aborted transaction: %+v, %v", results, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = engine.Open(directory)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer database.Close()
	results, err = database.Execute("SELECT balance FROM accounts WHERE id = 1")
	if err != nil || len(results[0].Rows) != 1 || results[0].Rows[0][0].String() != "100" {
		t.Fatalf("committed transaction did not survive restart: %+v, %v", results, err)
	}
}

func TestInnerJoinExecution(t *testing.T) {
	database, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	results, err := database.Execute(`
        CREATE TABLE users (id BIGINT PRIMARY KEY, name TEXT NOT NULL);
        CREATE TABLE orders (id BIGINT PRIMARY KEY, user_id BIGINT NOT NULL, total INT NOT NULL);
        INSERT INTO users VALUES (1, 'Alice'), (2, 'Bob');
        INSERT INTO orders VALUES (10, 1, 50), (11, 1, 75), (12, 2, 20);
        SELECT users.name, orders.total
          FROM users JOIN orders ON users.id = orders.user_id
         WHERE orders.total >= 50
         ORDER BY orders.total DESC;
    `)
	if err != nil {
		t.Fatalf("execute join: %v", err)
	}
	joined := results[len(results)-1]
	if len(joined.Rows) != 2 || joined.Rows[0][0].String() != "Alice" || joined.Rows[0][1].String() != "75" || joined.Rows[1][1].String() != "50" {
		t.Fatalf("unexpected join result: %+v", joined.Rows)
	}
	if !strings.Contains(joined.Plan, "NestedLoopJoin") {
		t.Fatalf("join plan missing join operator:\n%s", joined.Plan)
	}
}

func TestSecondaryIndexMaintenanceUniquenessAndOptimizer(t *testing.T) {
	database, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	_, err = database.Execute(`
        CREATE TABLE users (id BIGINT PRIMARY KEY, email TEXT NOT NULL, country TEXT NOT NULL);
        INSERT INTO users VALUES (1, 'a@example.com', 'IN'), (2, 'b@example.com', 'IN'), (3, 'c@example.com', 'US');
        CREATE UNIQUE INDEX users_email_idx ON users(email);
        CREATE INDEX users_country_idx ON users(country);
    `)
	if err != nil {
		t.Fatalf("setup indexes: %v", err)
	}
	explanation, err := database.Explain("SELECT id FROM users WHERE email = 'b@example.com'")
	if err != nil || !strings.Contains(explanation, "secondary-index-scan") || !strings.Contains(explanation, "users_email_idx") {
		t.Fatalf("unexpected indexed plan:\n%s\nerr=%v", explanation, err)
	}
	results, err := database.Execute("SELECT id FROM users WHERE country = 'IN' ORDER BY id")
	if err != nil || len(results[0].Rows) != 2 || results[0].Rows[1][0].String() != "2" {
		t.Fatalf("indexed lookup results=%+v err=%v", results, err)
	}
	if _, err := database.Execute("INSERT INTO users VALUES (4, 'a@example.com', 'GB')"); err == nil {
		t.Fatal("expected unique index violation")
	}
	results, err = database.Execute("SELECT COUNT(*) FROM users")
	if err != nil || results[0].Rows[0][0].String() != "3" {
		t.Fatalf("failed unique insert changed rows: %+v err=%v", results, err)
	}
	if _, err := database.Execute("UPDATE users SET email = 'z@example.com' WHERE id = 2"); err != nil {
		t.Fatalf("update indexed column: %v", err)
	}
	results, err = database.Execute("SELECT id FROM users WHERE email = 'z@example.com'")
	if err != nil || len(results[0].Rows) != 1 || results[0].Rows[0][0].String() != "2" {
		t.Fatalf("updated index lookup=%+v err=%v", results, err)
	}
	if _, err := database.Execute("DELETE FROM users WHERE id = 2"); err != nil {
		t.Fatalf("delete indexed row: %v", err)
	}
	results, err = database.Execute("SELECT id FROM users WHERE email = 'z@example.com'")
	if err != nil || len(results[0].Rows) != 0 {
		t.Fatalf("deleted index entry still visible: %+v err=%v", results, err)
	}
}

func TestUniqueIndexBackfillRollsBackMetadata(t *testing.T) {
	database, err := engine.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	_, err = database.Execute(`
        CREATE TABLE users (id BIGINT PRIMARY KEY, email TEXT NOT NULL);
        INSERT INTO users VALUES (1, 'same@example.com'), (2, 'same@example.com');
    `)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := database.Execute("CREATE UNIQUE INDEX users_email_idx ON users(email)"); err == nil {
		t.Fatal("expected unique backfill to fail")
	}
	_, schema, _ := database.Catalog().EnsureDefaults()
	table, err := database.Catalog().GetTable(schema.ID, "users")
	if err != nil {
		t.Fatalf("get table: %v", err)
	}
	if len(table.Indexes) != 0 {
		t.Fatalf("failed backfill left index metadata: %+v", table.Indexes)
	}
}
