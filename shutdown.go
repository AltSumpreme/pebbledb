package pebbledb

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

func WaitForShutdown(cleanup func()) {

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
	log.Printf("Received shutdown signal, closing PebbleDB...")
	cleanup()
}

func Cleanup(store *Store) func() {
	return func() {
		store.Close()
		log.Println("Cleanup completed. PebbleDB is now closed.")
		os.Exit(0)
	}

}
