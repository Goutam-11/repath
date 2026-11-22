package store

import "sync"

type Store struct {
	mu   sync.Mutex
	data map[string]string
}

func New() *Store {
	return &Store{data: map[string]string{}}
}

func (s *Store) Get(id string) string {
	return s.data[id]
}

func (s *Store) Put(id, value string) {
	s.data[id] = value
}
