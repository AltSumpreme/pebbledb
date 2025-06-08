package tests

import (
	"pebbledb"
	"testing"
)

func TestSET_GET(t *testing.T) {
	store := pebbledb.NewStore()
	store.Set("testKey", "testValue")
	value, exists := store.Get("testKey")
	if !exists {
		t.Errorf("Expected key 'testKey' to exist, but it does not.")
	}
	if value != "testValue" {
		t.Errorf("Expected value 'testValue', got '%s'", value)
	}
}

func TestDELETE(t *testing.T) {
	store := pebbledb.NewStore()
	store.Set("testKey", "testValue")
	store.Delete("testKey")
	_, exists := store.Get("testKey")
	if exists {
		t.Errorf("Expected key 'testKey' to be deleted, but it still exists.")
	}
}
