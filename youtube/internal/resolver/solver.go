package resolver

import (
	_ "embed"
	"fmt"
	"strings"

	"github.com/dop251/goja"
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/token"

	"github.com/colespringer/waxtap/v3/waxerr"
)

// This file runs YouTube's own player to solve the signature and n-parameter
// transforms, instead of carving the transform functions out of base.js by
// regex. Modern players use global-variable obfuscation that defeats name and
// closure carving; executing the player's real descrambler sidesteps that whole
// class of locator failure (see jsvm.go for the per-call runtime).
//
// The pipeline mirrors yt-dlp's current solver:
//
//  1. parsePlayer  - parse base.js once and locate the player IIFE.
//  2. unwrapPlayer - hoist the IIFE body to global scope so its inner
//     definitions (the descrambler among them) are reachable.
//  3. findDescramblers - fingerprint the descrambler(s) by AST shape: a
//     top-level function whose direct body has an obj.method("alr","yes") call.
//  4. a per-candidate driver constructs the player's URL object, sets n / the
//     signature, invokes the lone transform method, and reads the result back.
//  5. consensus (jsvm.go) runs every candidate and requires exactly one distinct
//     valid result, so a false positive that returns a plausible-but-wrong value
//     is rejected.

// envStubSource is the browser-global stub the full player needs to load. It is
// compiled once; a malformed stub fails the build via MustCompile.
//
//go:embed env.js
var envStubSource string

var envStubProgram = goja.MustCompile("env.js", envStubSource, false)

// transformKind selects which value a solve produces. The same descrambler and
// driver handle both; only the input wiring and the field read back differ.
type transformKind int

const (
	kindN transformKind = iota
	kindSig
)

func (k transformKind) String() string {
	if k == kindSig {
		return "signature"
	}
	return "n"
}

// candidate is one descrambler the consensus solver will try. driver is the
// compiled URL-object driver with this descrambler's reference baked in; name and
// ref are retained for observability and tests.
type candidate struct {
	name   string
	ref    string // JS expression that resolves the descrambler after the player runs
	driver *goja.Program
}

// defSite is a top-level function definition found while walking the IIFE body:
// its name, its function literal (to fingerprint), and the JS reference that
// resolves it once the unwrapped player has run.
type defSite struct {
	name string
	fl   *ast.FunctionLiteral
	ref  string
}

// parsePlayer parses base.js once and locates the player IIFE, returning the
// IIFE's function literal and the name of its first parameter (the namespace
// object the player attaches definitions to, or "" for a parameterless IIFE). A
// body that does not match either known IIFE shape is a clean ErrCipherSolve.
func parsePlayer(js string) (*ast.FunctionLiteral, string, error) {
	prog, err := parser.ParseFile(nil, "", js, parser.IgnoreRegExpErrors)
	if err != nil {
		return nil, "", fmt.Errorf("%w: parse player: %v", waxerr.ErrCipherSolve, err)
	}
	iife := locateIIFE(prog)
	if iife == nil {
		return nil, "", fmt.Errorf("%w: player IIFE not found", waxerr.ErrCipherSolve)
	}
	return iife, iifeArgName(iife), nil
}

// locateIIFE finds the player's wrapping IIFE among the program's top-level
// statements, handling the two shapes YouTube emits:
//
//	var X={}; (function(g){...})(X);   - callee is the function literal
//	(function(){...}).call(this);      - callee is (function).call
func locateIIFE(prog *ast.Program) *ast.FunctionLiteral {
	for _, st := range prog.Body {
		es, ok := st.(*ast.ExpressionStatement)
		if !ok {
			continue
		}
		ce, ok := es.Expression.(*ast.CallExpression)
		if !ok {
			continue
		}
		switch callee := ce.Callee.(type) {
		case *ast.FunctionLiteral:
			return callee
		case *ast.DotExpression: // (function(){}).call(this)
			if fl, ok := callee.Left.(*ast.FunctionLiteral); ok {
				return fl
			}
		}
	}
	return nil
}

// iifeArgName returns the IIFE's first parameter name, the object the player
// hangs its definitions on. It returns "" for a parameterless IIFE (the
// .call(this) shape), where there is no namespace object to recreate.
func iifeArgName(iife *ast.FunctionLiteral) string {
	if iife.ParameterList != nil && len(iife.ParameterList.List) > 0 {
		if id, ok := iife.ParameterList.List[0].Target.(*ast.Identifier); ok {
			return id.Name.String()
		}
	}
	return ""
}

// unwrapPlayer hoists the IIFE body to global scope so the descrambler is
// reachable after the program runs. It slices the body's outer brace range and,
// when the IIFE took a namespace argument, prepends a fresh object bound to it.
// Slicing only the outer braces (not per-statement) sidesteps goja's Idx1()
// trailing-token imprecision; the program is compiled non-strict so a top-level
// 'use strict' directive inside the body does not make the whole program strict.
//
// The namespace object is prepended only when the IIFE actually has a parameter:
// a parameterless .call(this) player has no namespace to recreate, and prepending
// a guessed name could collide with a real top-level let/const of the same name
// (a duplicate-declaration SyntaxError that fails the whole player).
func unwrapPlayer(js string, iife *ast.FunctionLiteral, argName string) (string, error) {
	if iife.Body == nil {
		return "", fmt.Errorf("%w: player IIFE has no body", waxerr.ErrCipherSolve)
	}
	// file.Idx is 1-based, so int(LeftBrace) is the offset immediately after '{'
	// and int(RightBrace)-1 is the offset of '}'. The slice is the inner body.
	lb, rb := int(iife.Body.LeftBrace), int(iife.Body.RightBrace)
	if lb < 1 || rb-1 < lb || rb-1 > len(js) {
		return "", fmt.Errorf("%w: player IIFE body range invalid", waxerr.ErrCipherSolve)
	}
	body := js[lb : rb-1]
	if argName == "" {
		return body, nil
	}
	return "var " + argName + "={};\n" + body, nil
}

// findDescramblers walks the IIFE body's top-level function definitions and
// returns those whose body fingerprints as a descrambler (a direct
// obj.method("alr","yes") statement). Each is paired with a compiled driver that
// references it. Matching only a direct body statement (not nested calls)
// mirrors yt-dlp and avoids tagging a helper's enclosing function.
func findDescramblers(playerURL string, iife *ast.FunctionLiteral) []candidate {
	if iife.Body == nil {
		return nil
	}
	var cands []candidate
	for _, st := range iife.Body.List {
		for _, d := range funcDefs(st) {
			if !hasAlrYes(d.fl) {
				continue
			}
			driver, err := goja.Compile(playerURL+"#driver:"+d.name, driverSource(d.ref), false)
			if err != nil {
				continue // the template is static; a compile failure can only mean skip
			}
			cands = append(cands, candidate{name: d.name, ref: d.ref, driver: driver})
		}
	}
	return cands
}

// funcDefs returns the function definitions a single top-level statement
// introduces, across the forms a player uses: function X(){}, var/let/const
// X=function, X=function, and obj.X=function. Each carries the JS reference that
// resolves it after the unwrapped player runs.
func funcDefs(st ast.Statement) []defSite {
	switch s := st.(type) {
	case *ast.FunctionDeclaration:
		if s.Function != nil && s.Function.Name != nil {
			n := s.Function.Name.Name.String()
			return []defSite{{n, s.Function, n}}
		}
	case *ast.VariableStatement:
		return bindingFuncs(s.List)
	case *ast.LexicalDeclaration:
		return bindingFuncs(s.List)
	case *ast.ExpressionStatement:
		if ae, ok := s.Expression.(*ast.AssignExpression); ok && ae.Operator == token.ASSIGN {
			if fl, ok := ae.Right.(*ast.FunctionLiteral); ok {
				switch l := ae.Left.(type) {
				case *ast.Identifier:
					n := l.Name.String()
					return []defSite{{n, fl, n}}
				case *ast.DotExpression:
					if ref, ok := memberRef(l); ok {
						return []defSite{{l.Identifier.Name.String(), fl, ref}}
					}
				}
			}
		}
	}
	return nil
}

// bindingFuncs collects function-valued declarators from a var/let/const list.
// The reference is the bare binding name: it resolves whether the binding lands
// on the global object (var) or only in the global lexical scope (let/const),
// where globalThis[name] would be undefined.
func bindingFuncs(list []*ast.Binding) []defSite {
	var out []defSite
	for _, b := range list {
		fl, ok := b.Initializer.(*ast.FunctionLiteral)
		if !ok {
			continue
		}
		if id, ok := b.Target.(*ast.Identifier); ok {
			n := id.Name.String()
			out = append(out, defSite{n, fl, n})
		}
	}
	return out
}

// memberRef builds the reference for an obj.X = function definition. The receiver
// must resolve to a global once the player has run: a bare identifier (the IIFE
// namespace, or any top-level binding) is used verbatim, and `this` is the global
// object at top level. A deeper or computed receiver is unsupported, so the
// candidate is skipped rather than resolved to a wrong reference.
func memberRef(d *ast.DotExpression) (string, bool) {
	name := d.Identifier.Name.String()
	switch o := d.Left.(type) {
	case *ast.Identifier:
		return o.Name.String() + "." + name, true
	case *ast.ThisExpression:
		return "globalThis." + name, true
	}
	return "", false
}

// hasAlrYes reports whether a direct statement of fl's body is a member call
// obj.method("alr","yes") with exactly two string-literal arguments. This is the
// descrambler's fingerprint; the call is not searched recursively (yt-dlp
// parity), which keeps a nested alr/yes helper from tagging its enclosing
// function.
func hasAlrYes(fl *ast.FunctionLiteral) bool {
	if fl.Body == nil {
		return false
	}
	for _, st := range fl.Body.List {
		es, ok := st.(*ast.ExpressionStatement)
		if !ok {
			continue
		}
		ce, ok := es.Expression.(*ast.CallExpression)
		if !ok || len(ce.ArgumentList) != 2 {
			continue
		}
		a, ok1 := ce.ArgumentList[0].(*ast.StringLiteral)
		b, ok2 := ce.ArgumentList[1].(*ast.StringLiteral)
		if !ok1 || !ok2 || a.Value.String() != "alr" || b.Value.String() != "yes" {
			continue
		}
		switch ce.Callee.(type) {
		case *ast.DotExpression, *ast.BracketExpression:
			return true
		}
	}
	return false
}

// driverSource is yt-dlp's URL-object driver, parameterized by the descrambler
// reference expression. It reads the inputs from the globals __wt_n / __wt_sig
// (set per call) and returns {sig, n}; the caller reads the field it needs. The
// signature rides in as the descrambler's encoded third argument and is read back
// via get("s") + decodeURIComponent; n is applied with set("n") after
// construction. The transform is the lone callable prototype member that is not
// constructor/set/get/clone (the typeof guard skips a data property ordered ahead
// of it).
func driverSource(ref string) string {
	return `(function(){var DESC=` + ref + `;if(typeof DESC!=="function")return null;
var url=DESC("https://youtube.com/watch?v=waxtap","s",__wt_sig===undefined?undefined:encodeURIComponent(__wt_sig));
if(!url||typeof url.set!=="function")return null;
url.set("n",__wt_n);
var proto=Object.getPrototypeOf(url);
var keys=Object.keys(proto).concat(Object.getOwnPropertyNames(proto));
for(var i=0;i<keys.length;i++){var k=keys[i];if(["constructor","set","get","clone"].indexOf(k)<0&&typeof url[k]==="function"){url[k]();break;}}
var s=url.get("s");
return {sig:s?decodeURIComponent(s):null,n:url.get("n")};})()`
}

// runCandidate runs one candidate's driver in vm and returns the transformed
// value for kind. A returned error is the raw goja failure (the candidate threw
// or the VM was interrupted); the caller classifies an interrupt as fatal and a
// plain throw as "this candidate produced nothing." A null/undefined result or
// missing field yields ("", nil).
func runCandidate(vm *goja.Runtime, cand candidate, kind transformKind) (string, error) {
	res, err := vm.RunProgram(cand.driver)
	if err != nil {
		return "", err
	}
	if res == nil || goja.IsNull(res) || goja.IsUndefined(res) {
		return "", nil
	}
	obj := res.ToObject(vm)
	if obj == nil {
		return "", nil
	}
	field := "n"
	if kind == kindSig {
		field = "sig"
	}
	v := obj.Get(field)
	if v == nil || goja.IsNull(v) || goja.IsUndefined(v) {
		return "", nil
	}
	return v.String(), nil
}

// validResult pre-filters a candidate's output before consensus. Both transforms
// reject the empty/"undefined"/echo (no-op) cases; n additionally rejects
// YouTube's own "enhanced_except_...<n>" failure sentinel (which ends with the
// input). Filtering these no-ops keeps an echoing candidate from breaking
// consensus; one that returns a different wrong value is still caught there.
func validResult(kind transformKind, in, out string) bool {
	if out == "" || out == "undefined" || out == in {
		return false
	}
	if kind == kindN && strings.HasSuffix(out, in) {
		return false
	}
	return true
}
