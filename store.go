package pebbledb

type Store struct {
	// The store is a map of key-value pairs.
	data map[string]string
}

func NewStore() *Store {
	return &Store{
		data: make(map[string]string),
	}
}

func (s *Store) Set(key, value string) {
	// Set the value for the given key.
	s.data[key] = value
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
	}
	return exists
}
