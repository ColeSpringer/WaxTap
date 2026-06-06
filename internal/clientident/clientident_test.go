package clientident

import (
	"strings"
	"testing"
)

func TestUserAgent(t *testing.T) {
	cases := []struct {
		major int
		want  string // Chrome token expected in the UA
	}{
		{0, "Chrome/149.0.0.0"},    // default
		{151, "Chrome/151.0.0.0"},  // explicit override
		{999, "Chrome/999.0.0.0"},  // valid upper bound
		{-1, "Chrome/149.0.0.0"},   // invalid: use default
		{1000, "Chrome/149.0.0.0"}, // invalid: use default
	}
	for _, c := range cases {
		ua := UserAgent(c.major)
		if !strings.Contains(ua, c.want) {
			t.Errorf("UserAgent(%d) = %q, want it to contain %q", c.major, ua, c.want)
		}
		// A reduced desktop User-Agent uses zero for minor, build, and patch.
		if !strings.HasPrefix(ua, "Mozilla/5.0 (Windows NT 10.0; Win64; x64)") {
			t.Errorf("UserAgent(%d) = %q, want the reduced desktop prefix", c.major, ua)
		}
		if !strings.HasSuffix(ua, "Safari/537.36") {
			t.Errorf("UserAgent(%d) = %q, want the Safari suffix", c.major, ua)
		}
	}
}

func TestUserAgentDefaultMatchesConst(t *testing.T) {
	if got, want := UserAgent(0), UserAgent(DefaultChromeMajor); got != want {
		t.Errorf("UserAgent(0) = %q, want it to equal UserAgent(DefaultChromeMajor) = %q", got, want)
	}
}

func TestValidChromeMajor(t *testing.T) {
	for _, m := range []int{0, 149, 999} {
		if !ValidChromeMajor(m) {
			t.Errorf("ValidChromeMajor(%d) = false, want true", m)
		}
	}
	for _, m := range []int{-1, 1000} {
		if ValidChromeMajor(m) {
			t.Errorf("ValidChromeMajor(%d) = true, want false", m)
		}
	}
}
