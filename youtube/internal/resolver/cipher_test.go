package resolver

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func readBaseJS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/base.js")
	if err != nil {
		t.Fatalf("read base.js: %v", err)
	}
	return string(b)
}

func TestExtractSignatureSource(t *testing.T) {
	src, name, err := extractSignatureSource(readBaseJS(t))
	if err != nil {
		t.Fatal(err)
	}
	if name != "dcr" {
		t.Errorf("signature name = %q, want dcr", name)
	}
	// The helper object the function calls into must be bundled, or the snippet
	// would reference an undefined global at run time.
	if !strings.Contains(src, "Xq") {
		t.Errorf("signature source missing helper object Xq:\n%s", src)
	}
}

func TestExtractNSource(t *testing.T) {
	_, name, err := extractNSource(readBaseJS(t))
	if err != nil {
		t.Fatal(err)
	}
	if name != "nfn" {
		t.Errorf("n-function name = %q, want nfn", name)
	}
}

// TestDecipherGolden covers the resolver path against the authored base.js:
// locate, extract, compile, and run both transforms.
func TestDecipherGolden(t *testing.T) {
	prog := compilePlayerProgram("https://www.youtube.com/s/player/abcd/base.js", readBaseJS(t))
	if prog.sigErr != nil {
		t.Fatalf("signature extraction failed: %v", prog.sigErr)
	}
	if prog.nErr != nil {
		t.Fatalf("n extraction failed: %v", prog.nErr)
	}

	got, err := prog.decipherSignature(context.Background(), "ABCDEFGH", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != "GFEDH" {
		t.Errorf("decipherSignature(ABCDEFGH) = %q, want GFEDH", got)
	}

	gotN, err := prog.decodeN(context.Background(), "12345", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if gotN != "54321" {
		t.Errorf("decodeN(12345) = %q, want 54321", gotN)
	}
}

// TestExtractNSource_ArrayIndirection covers the modern indirection where the n
// function is referenced through a one-element array (var ARR=[realName]).
func TestExtractNSource_ArrayIndirection(t *testing.T) {
	js := `var Pn=[dec];` +
		`function dec(a){var b=a.split("");b.reverse();return b.join("")}` +
		`;if(qx)(qx.get("n"))&&(qx=Pn[0](qx.get("n")));`

	src, name, err := extractNSource(js)
	if err != nil {
		t.Fatal(err)
	}
	if name != "dec" {
		t.Errorf("resolved n name = %q, want dec", name)
	}

	prog, cerr := compileSource(t, "n", src)
	if cerr != nil {
		t.Fatal(cerr)
	}
	got, err := runTransform(context.Background(), prog, name, "abcde", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != "edcba" {
		t.Errorf("decoded = %q, want edcba", got)
	}
}

func TestExtractFunctionDef_BraceAware(t *testing.T) {
	// A string literal containing an unbalanced brace must not end the body early.
	js := `weird=function(a){var s="}}}";return a+s}`
	def, ok := extractFunctionDef(js, "weird")
	if !ok {
		t.Fatal("function not found")
	}
	if !strings.HasSuffix(def, `return a+s}`) {
		t.Errorf("brace matching stopped early: %q", def)
	}
}

func TestExtractSignatureSource_NotFound(t *testing.T) {
	if _, _, err := extractSignatureSource("var x = 1;"); err == nil {
		t.Fatal("expected error when no signature function is present")
	}
}
