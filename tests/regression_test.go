package tests

import (
	"os"
	"path/filepath"
	"pebbledb"
	"pebbledb/db"
	"pebbledb/parser"
	"pebbledb/storage"
	"testing"
)

func TestInvalidInsertReturnsError(t *testing.T) {
	database := db.NewDatabase()
	if err := database.CreateTable("users", []db.Column{
		{Name: "id", Type: db.TypeInt},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}

	if err := database.InsertValue("users", []string{"not-an-int"}); err == nil {
		t.Fatal("expected invalid INT insert to return an error")
	}
}

func TestParserHandlesWhitespaceAndQuotedValues(t *testing.T) {
	cmd, err := parser.Parse(`INSERT TO TABLE users ( id, name ) FROM VALUES ( 1, "Alice Smith" )`)
	if err != nil {
		t.Fatalf("parse insert: %v", err)
	}
	if cmd.Tablename != "users" {
		t.Fatalf("expected users table, got %s", cmd.Tablename)
	}
	if len(cmd.Columns) != 2 || cmd.Columns[1].Name != "name" {
		t.Fatalf("unexpected columns: %+v", cmd.Columns)
	}
	if len(cmd.Values) != 2 || cmd.Values[1] != "Alice Smith" {
		t.Fatalf("unexpected values: %+v", cmd.Values)
	}
}

func TestIdentifierCasingIsNormalized(t *testing.T) {
	engine, err := pebbledb.NewEngine(t.TempDir())
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	runCommand(t, engine, "CREATE TABLE Users ID:INT Name:STRING")
	runCommand(t, engine, `INSERT TO TABLE users ( NAME, id ) VALUES ( "Alice Smith", 7 )`)

	cmd, err := parser.Parse("SELECT name FROM USERS")
	if err != nil {
		t.Fatalf("parse select: %v", err)
	}
	result := engine.Execute(cmd)
	if result.Error != nil {
		t.Fatalf("select: %v", result.Error)
	}
	if len(result.Rows) != 1 || result.Rows[0]["name"] != "Alice Smith" {
		t.Fatalf("unexpected rows: %+v", result.Rows)
	}
}

func TestDropRemovesPersistedFilesOnReload(t *testing.T) {
	dataDir := t.TempDir()
	engine, err := pebbledb.NewEngine(dataDir)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	runCommand(t, engine, "CREATE TABLE users id:INT name:STRING")
	runCommand(t, engine, "INSERT TO TABLE users (id,name) VALUES (1,Alice)")
	runCommand(t, engine, "DROP TABLE users")

	loaded, err := storage.LoadFromDisk(dataDir)
	if err != nil {
		t.Fatalf("load from disk: %v", err)
	}
	if _, err := loaded.GetTable("users"); err == nil {
		t.Fatal("expected dropped table to stay dropped after reload")
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".db" || filepath.Ext(entry.Name()) == ".json" {
			t.Fatalf("expected data files to be removed after drop, found %s", entry.Name())
		}
	}
}

func TestSaveToDiskReportsInvalidDataDirectory(t *testing.T) {
	database := db.NewDatabase()
	dataDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataDir, []byte("file"), 0664); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	if err := storage.SaveToDisk(database, dataDir); err == nil {
		t.Fatal("expected SaveToDisk to fail when data directory is a file")
	}
}

func TestLoadFromDiskRejectsCorruptedPageFile(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "users.meta.json"), []byte(`[{"Name":"id","Type":"INT"}]`), 0664); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "users_0.db"), []byte("short"), 0664); err != nil {
		t.Fatalf("write corrupt page: %v", err)
	}

	if _, err := storage.LoadFromDisk(dataDir); err == nil {
		t.Fatal("expected corrupted page file to return an error")
	}
}

func TestEmptyTablePersistsAcrossReload(t *testing.T) {
	dataDir := t.TempDir()
	database := db.NewDatabase()
	if err := database.CreateTable("users", []db.Column{{Name: "id", Type: db.TypeInt}}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := storage.SaveToDisk(database, dataDir); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := storage.LoadFromDisk(dataDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := loaded.GetTable("users"); err != nil {
		t.Fatalf("expected empty table to reload: %v", err)
	}
}

func runCommand(t *testing.T, engine *pebbledb.Engine, input string) {
	t.Helper()
	cmd, err := parser.Parse(input)
	if err != nil {
		t.Fatalf("parse %q: %v", input, err)
	}
	result := engine.Execute(cmd)
	if result.Error != nil {
		t.Fatalf("execute %q: %v", input, result.Error)
	}
}
