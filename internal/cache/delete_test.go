package cache

import "testing"

func TestDelete(t *testing.T) {
	s := NewStore[string](Options{})
	s.Put("k", "v")
	if _, ok := s.Get("k"); !ok {
		t.Fatal("value not stored")
	}
	s.Delete("k")
	if v, ok := s.Get("k"); ok {
		t.Errorf("Get after Delete = %q, want miss", v)
	}
	s.Delete("absent") // absent key must not panic
	if got := s.Len(); got != 0 {
		t.Errorf("Len = %d, want 0", got)
	}
}
