package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// readme returns the repo's README, which sits one level up from cmd/waxtap.
func readme(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var readmeExitRow = regexp.MustCompile(`(?m)^\| (\d+) \|`)

// The exit-code table is hand-kept in two places. They agree today; this keeps
// them agreeing.
func TestExitCodeTableMatchesReadme(t *testing.T) {
	doc := readme(t)
	documented := map[int]bool{}
	for _, m := range readmeExitRow.FindAllStringSubmatch(doc, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("unreadable exit-code row %q", m[0])
		}
		documented[n] = true
	}
	if len(documented) == 0 {
		t.Fatal("no exit-code rows found in README; the pin is reading the wrong table")
	}
	for _, e := range exitCodeTable {
		if !documented[e.Code] {
			t.Errorf("exit code %d is in exitCodeTable and not in README", e.Code)
		}
		delete(documented, e.Code)
	}
	for code := range documented {
		t.Errorf("exit code %d is in README and not in exitCodeTable", code)
	}
}

// Every note code the CLI can record is documented, the way the warning codes
// are: both are vocabularies a --json consumer matches on.
func TestNoteCodesDocumented(t *testing.T) {
	doc := readme(t)
	for _, c := range allNoteCodes {
		if !strings.Contains(doc, "`"+string(c)+"`") {
			t.Errorf("note %q is not documented in README", c)
		}
	}
}

// cut is a top-level command README never mentioned, so its flags had no
// documentation outside --help.
func TestReadmeDocumentsCut(t *testing.T) {
	doc := readme(t)
	for _, want := range []string{"waxtap cut ", "`--cut-range", "`--cut-mode", "`--crossfade"} {
		if !strings.Contains(doc, want) {
			t.Errorf("README does not document %q", want)
		}
	}
}

// allNoteCodes is hand-kept beside the constants it lists, so it is pinned to
// them: a code added to the const block and forgotten here would otherwise go
// undocumented with nothing to say so.
func TestAllNoteCodesMatchesTheConstants(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "output.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// The block is typed noteCode; nothing else here is.
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "noteCode" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				declared[strings.Trim(lit.Value, `"`)] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no noteCode constants found; the pin is reading the wrong file")
	}
	listed := map[string]bool{}
	for _, c := range allNoteCodes {
		if listed[string(c)] {
			t.Errorf("note %q is listed twice in allNoteCodes", c)
		}
		listed[string(c)] = true
		if !declared[string(c)] {
			t.Errorf("allNoteCodes holds %q, which no noteCode constant spells", c)
		}
	}
	for code := range declared {
		if !listed[code] {
			t.Errorf("note %q is a constant and not in allNoteCodes; add it there and to README", code)
		}
	}
}
