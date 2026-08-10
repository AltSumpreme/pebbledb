package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"pebbledb/server/pgwire"
	"pebbledb/sql/engine"
)

func main() {
	dataDirectory := flag.String("data", "pebbledb-data", "database storage directory")
	address := flag.String("listen", "127.0.0.1:5432", "PostgreSQL listen address")
	auth := flag.String("auth", "trust", "authentication mode: trust or password")
	user := flag.String("user", "", "required PostgreSQL user (empty accepts any user)")
	password := flag.String("password", "", "cleartext authentication password")
	flag.Parse()

	database, err := engine.Open(*dataDirectory)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer database.Close()
	authMode := pgwire.TrustAuth
	if strings.EqualFold(*auth, "password") {
		authMode = pgwire.CleartextPasswordAuth
	} else if !strings.EqualFold(*auth, "trust") {
		log.Fatalf("unsupported authentication mode %q", *auth)
	}
	server, err := pgwire.New(database, pgwire.Config{AuthMode: authMode, User: *user, Password: *password})
	if err != nil {
		log.Fatalf("configure PostgreSQL server: %v", err)
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		log.Fatalf("listen on %s: %v", *address, err)
	}
	log.Printf("PebbleDB PostgreSQL endpoint listening on %s", listener.Addr())

	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-serveDone:
		if err != nil {
			log.Fatalf("serve: %v", err)
		}
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}
