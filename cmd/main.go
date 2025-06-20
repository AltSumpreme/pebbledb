package main

import (
	"log"
	"pebbledb"
	"pebbledb/repl"
)

func main() {

	engine, err := pebbledb.NewEngine()
	if err != nil {
		log.Fatal("Failed to initialize PebbleDB engine:", err)
	}

	repl.ReplInit(engine.DB)
}
