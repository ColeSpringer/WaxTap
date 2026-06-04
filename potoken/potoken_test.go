package potoken

import "testing"

func TestParseScopeRoundTrip(t *testing.T) {
	for _, s := range []Scope{ScopeNone, ScopePlayer, ScopeGVS, ScopeSubtitles} {
		got, err := ParseScope(s.String())
		if err != nil {
			t.Errorf("ParseScope(%q): %v", s.String(), err)
			continue
		}
		if got != s {
			t.Errorf("ParseScope(%q) = %v, want %v", s.String(), got, s)
		}
	}
}

func TestParseScopeAcceptsEmptyAndCase(t *testing.T) {
	cases := map[string]Scope{
		"":          ScopeNone,
		"  none  ":  ScopeNone,
		"GVS":       ScopeGVS,
		"Player":    ScopePlayer,
		"SUBTITLES": ScopeSubtitles,
	}
	for in, want := range cases {
		got, err := ParseScope(in)
		if err != nil {
			t.Errorf("ParseScope(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseScope(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseScopeRejectsUnknown(t *testing.T) {
	if _, err := ParseScope("nonsense"); err == nil {
		t.Fatal("expected an error for an unknown scope")
	}
}
