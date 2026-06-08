package resolver

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"
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
	src, name, err := extractNSource(readBaseJS(t))
	if err != nil {
		t.Fatal(err)
	}
	if name != "nfn" {
		t.Errorf("n-function name = %q, want nfn", name)
	}
	// nfn references the global lookup table nDigits; the closure must bundle it
	// or the snippet would ReferenceError at run time.
	if !strings.Contains(src, "nDigits") {
		t.Errorf("n source missing dependency nDigits:\n%s", src)
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
	def, _, ok := extractFunctionDef(js, "weird")
	if !ok {
		t.Fatal("function not found")
	}
	if !strings.HasSuffix(def, `return a+s}`) {
		t.Errorf("brace matching stopped early: %q", def)
	}
}

// TestFindTopLevelDefinition_NameBoundary checks that looking up a short global
// does not match a longer identifier or a property name that ends with it. Real
// base.js is full of `X.yy=function` method assignments and reused 2-char names,
// so an unbounded `wK=function` search would otherwise return a corrupted def and
// mask the real `var wK="..."`.
func TestFindTopLevelDefinition_NameBoundary(t *testing.T) {
	cases := []struct {
		name string
		js   string
	}{
		{"longer identifier suffix", `var AwK=function(z){return z};var wK="0123456789";`},
		{"property method assignment", `var P={};P.wK=function(z){return z};var wK="0123456789";`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The decoy is not a real definition of wK, so the function-def lookup
			// must miss it and fall through to the var initializer.
			if _, _, ok := extractFunctionDef(tc.js, "wK"); ok {
				t.Errorf("extractFunctionDef matched a decoy ending in wK")
			}
			def, _, ok := findTopLevelDefinition(tc.js, "wK")
			if !ok {
				t.Fatal("wK not located")
			}
			if !strings.HasPrefix(def, `var wK=`) {
				t.Errorf("decoy masked the real global: def=%q", def)
			}
		})
	}
}

func TestExtractSignatureSource_NotFound(t *testing.T) {
	if _, _, err := extractSignatureSource("var x = 1;"); err == nil {
		t.Fatal("expected error when no signature function is present")
	}
}

func TestExtractSignatureTimestamp(t *testing.T) {
	sts, ok := extractSignatureTimestamp(readBaseJS(t))
	if !ok {
		t.Fatal("signature timestamp not found in base.js fixture")
	}
	if sts != 19834 {
		t.Errorf("signature timestamp = %d, want 19834", sts)
	}
}

// TestExtractClosure_TransitiveDependencies reproduces the embedded-player shape
// that broke v1.4.0: an n-function (zL) that calls a helper function (hF) that
// reads a string global (wK). The body-only extractor bundled only zL and threw
// "wK is not defined" at run time; the closure walker must bundle all three.
func TestExtractClosure_TransitiveDependencies(t *testing.T) {
	js := `var wK="9876543210";` +
		`function hF(x){return wK.charAt(parseInt(x,10))}` +
		`function zL(a){var b=a.split("");for(var i=0;i<b.length;i++)b[i]=hF(b[i]);return b.join("")}` +
		`;if(cd)(cd.get("n"))&&(cd=zL(cd.get("n")));`

	src, name, err := extractNSource(js)
	if err != nil {
		t.Fatal(err)
	}
	if name != "zL" {
		t.Errorf("n name = %q, want zL", name)
	}
	for _, dep := range []string{"zL", "hF", "wK"} {
		if !strings.Contains(src, dep) {
			t.Errorf("closure missing dependency %q:\n%s", dep, src)
		}
	}

	prog, cerr := compileSource(t, "n", src)
	if cerr != nil {
		t.Fatal(cerr)
	}
	// Running (not just compiling) is what surfaces a missing dependency: an
	// undefined global throws only at call time, not at compile time.
	got, err := runTransform(context.Background(), prog, name, "123", time.Second)
	if err != nil {
		t.Fatalf("decodeN raised (likely a missing closure dependency): %v", err)
	}
	if got != "876" { // 1->8, 2->7, 3->6 through the 9876543210 table
		t.Errorf("decoded = %q, want 876", got)
	}
}

// TestExtractClosure_ExcludesLocals checks that local bindings inside the
// transform are not mistaken for top-level dependencies and pulled in. The decoy
// top-level definitions reference undefined globals, so a wrong pull would throw.
func TestExtractClosure_ExcludesLocals(t *testing.T) {
	js := `var b=missingGlobalOne;` + // decoy: same name as a local in zL
		`function c(){return missingGlobalTwo}` + // decoy: same name as a local in zL
		`var wK="0123456789";` +
		`function zL(a){var b=a.split("");var c=wK;for(var i=0;i<b.length;i++)b[i]=c.charAt(parseInt(b[i],10));return b.join("")}` +
		`;if(cd)(cd.get("n"))&&(cd=zL(cd.get("n")));`

	src, name, err := extractNSource(js)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src, "missingGlobal") {
		t.Errorf("a decoy top-level definition was pulled into the closure:\n%s", src)
	}

	prog, cerr := compileSource(t, "n", src)
	if cerr != nil {
		t.Fatal(cerr)
	}
	got, err := runTransform(context.Background(), prog, name, "12345", time.Second)
	if err != nil {
		t.Fatalf("decodeN raised (a decoy was likely bundled): %v", err)
	}
	if got != "12345" { // identity table, no reversal
		t.Errorf("decoded = %q, want 12345", got)
	}
}

// TestFreeIdentifiers covers the AST scope analysis directly: the cases a
// hand-rolled string scanner gets wrong (default parameters, destructuring,
// shadowing, member names vs object keys).
func TestFreeIdentifiers(t *testing.T) {
	cases := []struct {
		name string
		def  string
		want string // comma-joined, sorted
	}{
		{"default param is local; its default expr is free", `function zL(a,b=wK){return a+b}`, "wK"},
		{"object and array destructuring bind their targets", `function f(o){var {p,q}=o;var [r]=o;return p+q+r+ZZ}`, "ZZ"},
		{"a parameter shadows a same-named global", `function f(wK){return wK}`, ""},
		{"member names and object-literal keys are excluded", `function f(a){return a.charAt(0)+({k:a}).k}`, ""},
		{"a helper object is a free reference", `function f(a){return Xq.run(a,3)}`, "Xq"},
		{"object shorthand is a value reference", `var o=function(){return {wK}}`, "wK"},
		// A binding in one scope must not mask a free reference to the same name in
		// another: the nested param and the block-scoped let are local, the outer
		// wK is the dependency. A flat bound/ref set dropped wK in both.
		{"a nested-function param does not shadow an outer global", `function f(a){var r=wK.x(a);var g=function(wK){return wK};return g(r)}`, "wK"},
		{"a block-scoped let does not leak to the enclosing scope", `function f(a){if(a){let x=1;return x}return wK}`, "wK"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := freeIdentifiers(tc.def)
			if err != nil {
				t.Fatal(err)
			}
			if joined := strings.Join(got, ","); joined != tc.want {
				t.Errorf("freeIdentifiers(%q) = %q, want %q", tc.def, joined, tc.want)
			}
		})
	}
}

// TestExtractVarInit covers the two subtlest var-init cases: a helper object body
// with a depth>0 ';' must be captured whole, and a compound-assignment site must
// not be mistaken for the declaration.
func TestExtractVarInit(t *testing.T) {
	t.Run("nested semicolon does not truncate", func(t *testing.T) {
		js := `var Xq={sw:function(a,b){var c=a[0];a[0]=a[b];a[b]=c}};next=1;`
		def, _, ok := extractVarInit(js, "Xq")
		if !ok {
			t.Fatal("Xq not located")
		}
		if !strings.Contains(def, "a[b]=c") || !strings.HasSuffix(def, "}}") {
			t.Errorf("var-init truncated at a nested ';': %q", def)
		}
	})
	t.Run("compound assignment is not the declaration", func(t *testing.T) {
		js := `wK+=1;var wK="payload";`
		def, _, ok := extractVarInit(js, "wK")
		if !ok {
			t.Fatal("wK declaration not located")
		}
		if !strings.Contains(def, "payload") {
			t.Errorf("a compound-assignment site was taken as the declaration: %q", def)
		}
	})
}

// TestGojaES6Floor documents the ES6 constructs the closure pipeline depends on:
// goja's parser must parse them (scope analysis) and goja must compile them (the
// walker's output). A goja downgrade that drops any of these fails here rather
// than silently breaking extraction on a modern player.
func TestGojaES6Floor(t *testing.T) {
	const snippet = "var f=function(a){const g=(x)=>x??0;let {p=1}=a||{};let arr=[...a];return g(a?.b)+p+`v${p}`+arr.length}"
	if _, err := parser.ParseFile(nil, "", snippet, parser.IgnoreRegExpErrors); err != nil {
		t.Fatalf("goja parser rejected an ES6 snippet (parse floor regressed): %v", err)
	}
	if _, err := goja.Compile("es6", snippet, false); err != nil {
		t.Fatalf("goja compiler rejected an ES6 snippet (compile floor regressed): %v", err)
	}
	if _, err := freeIdentifiers(snippet); err != nil {
		t.Fatalf("freeIdentifiers failed on an ES6 snippet: %v", err)
	}
}

func TestExtractSignatureTimestamp_Variants(t *testing.T) {
	cases := []struct {
		name string
		js   string
		want int
		ok   bool
	}{
		{"signatureTimestamp colon", `a={signatureTimestamp:18000,b:1}`, 18000, true},
		{"sts short key", `{sts:17999}`, 17999, true},
		{"sts after comma", `{a:1,sts:16000}`, 16000, true},
		{"sts quoted key", `{"sts":17777}`, 17777, true},
		{"absent", `var x = 1; function f(){}`, 0, false},
		{"zero rejected", `{signatureTimestamp:0}`, 0, false},
		// Ignore assignments and member access, which may contain unrelated
		// timestamp values.
		{"assignment form ignored", `var x; signatureTimestamp = 20001;`, 0, false},
		{"stray signatureTimestamp member", `a.signatureTimestamp=20002;b()`, 0, false},
		{"stray member access", `a.sts=12345;b()`, 0, false},
		{"stray sts variable", `var sts = 99;`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractSignatureTimestamp(tc.js)
			if ok != tc.ok || got != tc.want {
				t.Errorf("extractSignatureTimestamp = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
