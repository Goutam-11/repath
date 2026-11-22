package store

import "testing"

func TestStoreGet(t *testing.T) {
	var s *Store = New()
	if s.Get("x") != "" {
		t.Fatal("unexpected")
	}
}
