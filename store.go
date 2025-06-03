package pebbledb

import (
	"bufio"
	"log"
	"os"
	"strings"
)

type Store struct {
	// The store is a map of key-value pairs.
	data    map[string]string
	logFile *os.File
	logger  *log.Logger
}

func NewStore() *Store {

	file, err := os.OpenFile(("pebbledb.log"), os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		log.Fatal(err)
	}
	store := &Store{
		data:    make(map[string]string),
		logFile: file,
		logger:  log.New(file, "PebbleDB: ", log.LstdFlags),
	}
	_, err = file.Seek(0, 0)
	if err != nil {
		log.Fatal(err)
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.Fields(scanner.Text())
		if len(line) < 5 {
			continue
		}

		command := line[3]
		switch command {
		case "SET":
			// Parse the SET command from the log

			if len(line) >= 6 {
				key := line[4]
				value := line[5]
				store.data[key] = value
			}
		case "DELETE":
			//  Parse DELETE command from the log
			if len(line) >= 5 {

				key := line[4]
				delete(store.data, key)
			}
		}
	}
	return store
}
func (s *Store) Set(key, value string) {
	// Set the value for the given key.
	s.data[key] = value
	s.logger.Printf("SET %s %s", key, value)
	if err := s.logFile.Sync(); err != nil {
		s.logger.Printf("Error syncing log file: %v", err)
	}
}

func (s *Store) Get(key string) (string, bool) {
	// Get the value for the given key
	value, exists := s.data[key]
	if !exists {
		return "", false
	}
	return value, true
}

func (s *Store) Delete(key string) bool {
	// Delete the key-value pair for the given key
	_, exists := s.data[key]
	if exists {
		delete(s.data, key)
		s.logger.Printf("DELETE %s", key)
		if err := s.logFile.Sync(); err != nil {
			s.logger.Printf("Error syncing log file: %v", err)
		}
	}
	return exists
}
