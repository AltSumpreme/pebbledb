package engine_test

import (
	"testing"

	"pebbledb/sql/engine"
)

func BenchmarkIndexedPointSelect(b *testing.B) {
	database, err := engine.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Execute("CREATE TABLE users (id BIGINT PRIMARY KEY, email TEXT NOT NULL); INSERT INTO users VALUES (1, 'a@example.com'); CREATE UNIQUE INDEX users_email_idx ON users(email)"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		results, err := database.Execute("SELECT id FROM users WHERE email = 'a@example.com'")
		if err != nil || len(results[0].Rows) != 1 {
			b.Fatalf("select rows=%d err=%v", len(results[0].Rows), err)
		}
	}
}
