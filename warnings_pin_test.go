package waxtap

import (
	"os"
	"strings"
	"testing"
)

// knownWarningCodes is how many codes the vocabulary holds. A new code bumps
// it, which is what catches a constant added without a String arm: the walk
// below stops at the first code that spells "unknown", so a missing arm shows
// up here as a short count rather than as a warning nobody can match on.
const knownWarningCodes = 26

// The codes are a closed vocabulary: each spells itself, no two spell the
// same thing, and README documents every one. A consumer matching on the
// strings has nothing else to read.
func TestWarningCodesAreNamedAndDocumented(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(readme)

	seen := map[string]WarningCode{}
	n := 0
	for c := WarningCode(0); c.String() != "unknown"; c++ {
		name := c.String()
		if prev, dup := seen[name]; dup {
			t.Errorf("codes %d and %d both spell %q", prev, c, name)
		}
		seen[name] = c
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("warning %q is not documented in README", name)
		}
		n++
		if n > 200 {
			t.Fatal("the walk did not terminate; WarningCode.String has no default arm")
		}
	}
	if n != knownWarningCodes {
		t.Errorf("%d named warning codes, want %d; a new code needs a String arm, a README row, and this constant bumped", n, knownWarningCodes)
	}
}
