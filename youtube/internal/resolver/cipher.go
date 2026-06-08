package resolver

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"

	"github.com/colespringer/waxtap/waxerr"
)

// This file locates the signature and n-parameter transform functions inside
// YouTube's base.js. The transforms themselves are not reimplemented; their
// source is extracted and run in goja. In practice, maintenance is mostly adding
// locator patterns when YouTube changes the surrounding JavaScript.
//
// Extraction proceeds in two stages. First a locator pattern finds the transform
// function's name. Then extractClosure bundles the transitive closure of every
// top-level definition the function references (global variables, helper
// functions, helper objects), so the emitted snippet is self-contained and runs
// as-is in a goja runtime (see jsvm.go). freeIdentifiers parses each definition
// with goja and walks its AST scopes to tell real dependencies from the
// definition's own locals, parameters, and destructuring targets.

// signatureNamePatterns locate the signature transform's function name. Patterns
// are tried in order, and the first non-empty capture group wins. The definition
// pattern anchors on the split/join shape common to these transforms.
var signatureNamePatterns = []*regexp.Regexp{
	// Call site: c&&(c=FN(decodeURIComponent(c)))
	regexp.MustCompile(`[a-zA-Z0-9$]\s*&&\s*\(\s*[a-zA-Z0-9$]+\s*=\s*([a-zA-Z0-9$]{2,})\(decodeURIComponent\(`),
	// Definition: function FN(a){a=a.split("")  /  FN=function(a){a=a.split("")
	regexp.MustCompile(`(?:\bfunction\s+([a-zA-Z0-9$]{2,})|([a-zA-Z0-9$]{2,})\s*=\s*function)\s*\(\s*[a-zA-Z0-9$]+\s*\)\s*\{\s*[a-zA-Z0-9$]+\s*=\s*[a-zA-Z0-9$]+\.split\(\s*""\s*\)`),
	// Call site: encodeURIComponent(FN(  /  .sig||FN(
	regexp.MustCompile(`(?:encodeURIComponent\(|\.sig\|\|)\s*([a-zA-Z0-9$]{2,})\(`),
}

// nNamePatterns locate the n-parameter transform's function name. If group 2
// captures an array index, the function is referenced through an array and
// resolveArrayName maps it back to the real name.
var nNamePatterns = []*regexp.Regexp{
	// Call site: ...get("n"))&&(x=FN(x))  or  ...get("n"))&&(x=ARR[i](x))
	regexp.MustCompile(`[a-zA-Z0-9$]+\.get\(\s*"n"\s*\)\s*\)?\s*&&\s*\(?\s*[a-zA-Z0-9$]+\s*=\s*([a-zA-Z0-9$]+)(?:\[(\d+)\])?\(`),
	// Older form: (b=FN(c))&&  /  (b=ARR[i](c))&&
	regexp.MustCompile(`\(\s*[a-zA-Z0-9$]+\s*=\s*([a-zA-Z0-9$]+)(?:\[(\d+)\])?\(\s*[a-zA-Z0-9$]+\s*\)\s*\)\s*&&`),
}

// stsPatterns locate the signature timestamp in base.js. Current players use
// signatureTimestamp:<int>; older players may use sts:<int>. Only object-literal
// fields match. Assignments such as foo.signatureTimestamp=1 or foo.sts=1 are
// ignored because a false timestamp causes YouTube to reject the request.
var stsPatterns = []*regexp.Regexp{
	regexp.MustCompile(`signatureTimestamp\s*:\s*(\d+)`),
	regexp.MustCompile(`[{,]\s*"?sts"?\s*:\s*(\d+)`),
}

var identifierRe = regexp.MustCompile(`^[a-zA-Z_$][a-zA-Z0-9_$]*$`)

const (
	// maxClosureDefs caps the number of distinct definitions bundled into one
	// transform snippet. Real cipher closures are well under 15; 32 is headroom
	// that still catches a runaway or a false-dependency leak early.
	maxClosureDefs = 32
	// maxClosureBytes caps the total emitted snippet size.
	maxClosureBytes = 1 << 20
)

// extractSignatureSource returns a self-contained JavaScript snippet that defines
// the signature transform as a global function, plus the name to call. The
// snippet bundles the transitive closure of every top-level definition the
// function references (helper objects, helper functions, globals).
func extractSignatureSource(js string) (src, name string, err error) {
	name, ok := locateName(signatureNamePatterns, js)
	if !ok {
		return "", "", fmt.Errorf("%w: signature function name not found", waxerr.ErrCipherSolve)
	}
	src, err = extractClosure(js, name)
	if err != nil {
		return "", "", err
	}
	return src, name, nil
}

// extractNSource returns a self-contained snippet defining the n-parameter
// transform as a global function, plus the name to call. Like the signature
// snippet, it bundles the function's full dependency closure.
func extractNSource(js string) (src, name string, err error) {
	name, idx, ok := locateNameIndex(nNamePatterns, js)
	if !ok {
		return "", "", fmt.Errorf("%w: n-function name not found", waxerr.ErrCipherSolve)
	}
	if idx >= 0 {
		resolved, ok := resolveArrayName(js, name, idx)
		if !ok {
			return "", "", fmt.Errorf("%w: n-function array %q[%d] not resolvable", waxerr.ErrCipherSolve, name, idx)
		}
		name = resolved
	}
	src, err = extractClosure(js, name)
	if err != nil {
		return "", "", err
	}
	return src, name, nil
}

// extractClosure returns a self-contained snippet defining the located root
// function plus the transitive closure of the top-level definitions it depends
// on, emitted in source order. It walks dependencies breadth-first to a fixpoint:
// each definition's free identifiers (per freeIdentifiers) are looked up as
// further top-level definitions until none remain.
//
// A missing root, a parse failure on any snippet, or a closure that exceeds the
// caps yields a clean ErrCipherSolve. A partial closure is never emitted: a
// transform missing one dependency would not throw at compile time but would
// ReferenceError at run time and silently 403 the stream, so an incomplete
// closure must fail loudly here instead.
func extractClosure(js, name string) (string, error) {
	type definition struct {
		text   string
		offset int
	}
	root, off, ok := findTopLevelDefinition(js, name)
	if !ok {
		return "", fmt.Errorf("%w: %q body not found", waxerr.ErrCipherSolve, name)
	}

	collected := map[string]definition{name: {root, off}}
	queue := []string{name}
	total := len(root)

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		ids, err := freeIdentifiers(collected[cur].text)
		if err != nil {
			return "", fmt.Errorf("%w: parse %q definition: %v", waxerr.ErrCipherSolve, cur, err)
		}
		for _, id := range ids {
			if _, seen := collected[id]; seen {
				continue
			}
			if isReservedOrBuiltin(id) {
				continue
			}
			def, defOff, ok := findTopLevelDefinition(js, id)
			if !ok {
				continue // a builtin or runtime global with no definition to bundle
			}
			if len(collected) >= maxClosureDefs {
				return "", fmt.Errorf("%w: closure for %q exceeds %d definitions", waxerr.ErrCipherSolve, name, maxClosureDefs)
			}
			if total += len(def); total > maxClosureBytes {
				return "", fmt.Errorf("%w: closure for %q exceeds %d bytes", waxerr.ErrCipherSolve, name, maxClosureBytes)
			}
			collected[id] = definition{def, defOff}
			queue = append(queue, id)
		}
	}

	// Emit in source order. Function declarations hoist; load-time var
	// initializers (the string/array globals) are self-contained; cross-helper
	// calls run at call time, after the whole snippet has loaded. So the
	// minifier's own order is a faithful, dependency-correct order and no
	// topological sort is needed.
	defs := make([]definition, 0, len(collected))
	for _, d := range collected {
		defs = append(defs, d)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].offset < defs[j].offset })

	var b strings.Builder
	for _, d := range defs {
		b.WriteString(d.text)
		b.WriteString(";\n")
	}
	return b.String(), nil
}

// freeIdentifiers parses one isolated definition snippet with goja's own parser
// and returns the identifiers it references but never binds. The parser's typed
// nodes do what a hand-rolled scanner gets wrong: member names (the b in a.b),
// non-computed object-literal keys, and labels stay out of the result, while
// parameters, var/let/const, function names, catch parameters, and destructuring
// targets are recognized as bindings.
//
// Shadowing is resolved against a lexical scope stack (one frame per function,
// block, loop header, and catch clause), not one flat set: a name used as a
// global in an outer scope but shadowed by a local in a nested one must still be
// reported, and a flat set marks it bound everywhere and silently drops the
// dependency. Bindings are recorded as the walk reaches them rather than hoisted,
// so a reference preceding its own local declaration is reported free. That only
// over-collects, which findTopLevelDefinition discards when no top-level
// definition exists; it never drops a real dependency.
//
// Only small extracted cipher snippets are parsed here, never the whole multi-
// megabyte base.js: the snippets are goja-compatible (goja compiles them), while
// the full file's unrelated modern JS could choke goja's parser and break all
// extraction. A parse error is returned so extractClosure can fail cleanly,
// consistent with goja being unable to run the snippet anyway.
func freeIdentifiers(def string) ([]string, error) {
	prog, err := parser.ParseFile(nil, "", def, parser.IgnoreRegExpErrors)
	if err != nil {
		return nil, err
	}
	c := &idCollector{scopes: []map[string]struct{}{{}}, free: map[string]struct{}{}}
	for _, s := range prog.Body {
		c.walk(s)
	}
	free := make([]string, 0, len(c.free))
	for id := range c.free {
		free = append(free, id)
	}
	sort.Strings(free)
	return free, nil
}

// idCollector accumulates the free identifier names of a snippet during an AST
// walk. scopes is a lexical scope stack (innermost last); a reference to a name
// bound by no open scope is free.
type idCollector struct {
	scopes []map[string]struct{}
	free   map[string]struct{}
}

func (c *idCollector) push() { c.scopes = append(c.scopes, map[string]struct{}{}) }
func (c *idCollector) pop()  { c.scopes = c.scopes[:len(c.scopes)-1] }

// bind records name in the innermost open scope.
func (c *idCollector) bind(name string) { c.scopes[len(c.scopes)-1][name] = struct{}{} }

// ref records a reference: free unless some open scope already binds name.
func (c *idCollector) ref(name string) {
	for _, s := range c.scopes {
		if _, ok := s[name]; ok {
			return
		}
	}
	c.free[name] = struct{}{}
}

// walk visits a node in reference position, recording identifier references and
// recursing into children. Binding constructs route their targets through
// bindTarget instead.
func (c *idCollector) walk(n ast.Node) {
	switch e := n.(type) {
	case nil:
		return

	case *ast.Identifier:
		c.ref(e.Name.String())

	// Expressions.
	case *ast.Binding:
		c.bindTarget(e.Target)
		c.walk(e.Initializer)
	case *ast.YieldExpression:
		c.walk(e.Argument)
	case *ast.AwaitExpression:
		c.walk(e.Argument)
	case *ast.ArrayLiteral:
		for _, v := range e.Value {
			c.walk(v)
		}
	case *ast.ArrayPattern: // destructuring assignment target (not a declaration)
		for _, el := range e.Elements {
			c.walk(el)
		}
		c.walk(e.Rest)
	case *ast.ObjectPattern:
		for _, p := range e.Properties {
			c.walk(p)
		}
		c.walk(e.Rest)
	case *ast.AssignExpression:
		c.walk(e.Left)
		c.walk(e.Right)
	case *ast.BinaryExpression:
		c.walk(e.Left)
		c.walk(e.Right)
	case *ast.BracketExpression:
		c.walk(e.Left)
		c.walk(e.Member)
	case *ast.CallExpression:
		c.walk(e.Callee)
		for _, a := range e.ArgumentList {
			c.walk(a)
		}
	case *ast.ConditionalExpression:
		c.walk(e.Test)
		c.walk(e.Consequent)
		c.walk(e.Alternate)
	case *ast.DotExpression:
		c.walk(e.Left) // e.Identifier is a property name, not a reference
	case *ast.PrivateDotExpression:
		c.walk(e.Left) // e.Identifier is a private member name, not a reference
	case *ast.OptionalChain:
		c.walk(e.Expression)
	case *ast.Optional:
		c.walk(e.Expression)
	case *ast.FunctionLiteral:
		c.walkFunction(e.Name, e.ParameterList, e.Body)
	case *ast.ArrowFunctionLiteral:
		c.walkFunction(nil, e.ParameterList, e.Body)
	case *ast.ClassLiteral:
		if e.Name != nil {
			c.bind(e.Name.Name.String())
		}
		c.walk(e.SuperClass)
		for _, ce := range e.Body {
			c.walk(ce)
		}
	case *ast.NewExpression:
		c.walk(e.Callee)
		for _, a := range e.ArgumentList {
			c.walk(a)
		}
	case *ast.ObjectLiteral:
		for _, p := range e.Value {
			c.walk(p)
		}
	case *ast.PropertyShort:
		c.ref(e.Name.Name.String()) // {x} is shorthand for {x: x}: x is a reference
		c.walk(e.Initializer)
	case *ast.PropertyKeyed:
		if e.Computed {
			c.walk(e.Key)
		}
		c.walk(e.Value)
	case *ast.SpreadElement:
		c.walk(e.Expression)
	case *ast.SequenceExpression:
		for _, x := range e.Sequence {
			c.walk(x)
		}
	case *ast.TemplateLiteral:
		c.walk(e.Tag)
		for _, x := range e.Expressions {
			c.walk(x)
		}
	case *ast.UnaryExpression:
		c.walk(e.Operand)
	case *ast.ExpressionBody:
		c.walk(e.Expression)

	// Statements.
	case *ast.BlockStatement:
		c.push()
		for _, s := range e.List {
			c.walk(s)
		}
		c.pop()
	case *ast.CaseStatement:
		c.walk(e.Test)
		for _, s := range e.Consequent {
			c.walk(s)
		}
	case *ast.CatchStatement:
		c.push() // the catch parameter is scoped to the catch clause
		c.bindTarget(e.Parameter)
		c.walk(e.Body)
		c.pop()
	case *ast.DoWhileStatement:
		c.walk(e.Test)
		c.walk(e.Body)
	case *ast.ExpressionStatement:
		c.walk(e.Expression)
	case *ast.ForInStatement:
		c.push()         // a let/const/var loop header is scoped to the loop
		c.walk(e.Source) // the iterated source is evaluated before the loop var binds
		c.walk(e.Into)
		c.walk(e.Body)
		c.pop()
	case *ast.ForOfStatement:
		c.push()
		c.walk(e.Source)
		c.walk(e.Into)
		c.walk(e.Body)
		c.pop()
	case *ast.ForStatement:
		c.push()
		c.walk(e.Initializer)
		c.walk(e.Test)
		c.walk(e.Update)
		c.walk(e.Body)
		c.pop()
	case *ast.IfStatement:
		c.walk(e.Test)
		c.walk(e.Consequent)
		c.walk(e.Alternate)
	case *ast.LabelledStatement:
		c.walk(e.Statement) // e.Label is a statement label, not a variable
	case *ast.ReturnStatement:
		c.walk(e.Argument)
	case *ast.SwitchStatement:
		c.walk(e.Discriminant)
		c.push() // the switch body is one block scope shared by the cases
		for _, cs := range e.Body {
			c.walk(cs)
		}
		c.pop()
	case *ast.ThrowStatement:
		c.walk(e.Argument)
	case *ast.TryStatement:
		c.walk(e.Body)
		if e.Catch != nil {
			c.walk(e.Catch)
		}
		if e.Finally != nil {
			c.walk(e.Finally)
		}
	case *ast.VariableStatement:
		for _, b := range e.List {
			c.walk(b)
		}
	case *ast.LexicalDeclaration:
		for _, b := range e.List {
			c.walk(b)
		}
	case *ast.WhileStatement:
		c.walk(e.Test)
		c.walk(e.Body)
	case *ast.WithStatement:
		c.walk(e.Object)
		c.walk(e.Body)
	case *ast.FunctionDeclaration:
		// A declaration's name belongs to the enclosing scope (so siblings can
		// call it); walkFunction also binds it inside the function's own scope.
		if e.Function != nil && e.Function.Name != nil {
			c.bind(e.Function.Name.Name.String())
		}
		c.walk(e.Function)
	case *ast.ClassDeclaration:
		if e.Class != nil && e.Class.Name != nil {
			c.bind(e.Class.Name.Name.String())
		}
		c.walk(e.Class)

	// for-loop initializer / iterator pieces.
	case *ast.ForLoopInitializerExpression:
		c.walk(e.Expression)
	case *ast.ForLoopInitializerVarDeclList:
		for _, b := range e.List {
			c.walk(b)
		}
	case *ast.ForLoopInitializerLexicalDecl:
		for _, b := range e.LexicalDeclaration.List {
			c.walk(b)
		}
	case *ast.ForIntoVar:
		c.walk(e.Binding)
	case *ast.ForDeclaration:
		c.bindTarget(e.Target)
	case *ast.ForIntoExpression:
		c.walk(e.Expression)

	// Class elements.
	case *ast.FieldDefinition:
		if e.Computed {
			c.walk(e.Key)
		}
		c.walk(e.Initializer)
	case *ast.MethodDefinition:
		if e.Computed {
			c.walk(e.Key)
		}
		c.walk(e.Body)
	case *ast.ClassStaticBlock:
		c.walk(e.Block)
	}
	// Remaining node types (literals, this/super, debugger, etc.) carry no
	// identifier references and need no traversal.
}

// walkFunction opens a scope for the function, records its name and parameters as
// bindings in that scope, walks any default-parameter expressions as references,
// and walks the body. Binding the name here covers a named function expression's
// self-reference; for a declaration the enclosing scope is bound separately.
func (c *idCollector) walkFunction(name *ast.Identifier, params *ast.ParameterList, body ast.Node) {
	c.push()
	defer c.pop()
	if name != nil {
		c.bind(name.Name.String())
	}
	if params != nil {
		for _, b := range params.List {
			c.bindTarget(b.Target)
			c.walk(b.Initializer) // default value, evaluated in the function scope
		}
		c.bindTarget(params.Rest)
	}
	c.walk(body)
}

// bindTarget records every name introduced by a binding target (a plain
// identifier or a destructuring pattern) and walks default-value expressions,
// which are references rather than bindings.
func (c *idCollector) bindTarget(t ast.Node) {
	switch b := t.(type) {
	case nil:
		return
	case *ast.Identifier:
		c.bind(b.Name.String())
	case *ast.Binding:
		c.bindTarget(b.Target)
		c.walk(b.Initializer)
	case *ast.ObjectPattern:
		for _, p := range b.Properties {
			switch p := p.(type) {
			case *ast.PropertyShort:
				c.bind(p.Name.Name.String())
				c.walk(p.Initializer)
			case *ast.PropertyKeyed:
				if p.Computed {
					c.walk(p.Key)
				}
				c.bindTarget(p.Value)
			case *ast.SpreadElement:
				c.bindTarget(p.Expression)
			}
		}
		c.bindTarget(b.Rest)
	case *ast.ArrayPattern:
		for _, el := range b.Elements {
			c.bindTarget(el)
		}
		c.bindTarget(b.Rest)
	case *ast.SpreadElement:
		c.bindTarget(b.Expression)
	}
}

// extractSignatureTimestamp returns the signature timestamp embedded in base.js.
// It reports false when no recognized pattern matches.
func extractSignatureTimestamp(js string) (int, bool) {
	for _, re := range stsPatterns {
		if m := re.FindStringSubmatch(js); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

// locateName returns the first non-empty capture group produced by any pattern,
// in pattern order.
func locateName(patterns []*regexp.Regexp, js string) (string, bool) {
	for _, re := range patterns {
		if m := re.FindStringSubmatch(js); m != nil {
			for _, g := range m[1:] {
				if g != "" {
					return g, true
				}
			}
		}
	}
	return "", false
}

// locateNameIndex is like locateName but also reports an optional array index
// (capture group 2). idx is -1 when no index was captured.
func locateNameIndex(patterns []*regexp.Regexp, js string) (name string, idx int, ok bool) {
	for _, re := range patterns {
		m := re.FindStringSubmatch(js)
		if m == nil || m[1] == "" {
			continue
		}
		idx = -1
		if len(m) > 2 && m[2] != "" {
			if n, err := strconv.Atoi(m[2]); err == nil {
				idx = n
			}
		}
		return m[1], idx, true
	}
	return "", -1, false
}

// findTopLevelDefinition locates name's definition text and start offset by
// string search, trying a function definition first and then a variable
// initializer. It reports false when no top-level definition exists, which the
// closure walker treats as a builtin or runtime global to drop. String search
// (not full-file parsing) is what bounds goja's parser exposure to the small,
// goja-compatible snippets it can handle.
func findTopLevelDefinition(js, name string) (def string, offset int, ok bool) {
	if def, off, ok := extractFunctionDef(js, name); ok {
		return def, off, true
	}
	return extractVarInit(js, name)
}

// extractFunctionDef returns the named function definition and its start offset.
// Forms accepted here are "function FN(...){...}" and "FN=function(...){...}".
//
// The "FN=function" form needs a left identifier boundary, the same one
// extractVarInit uses: without it a lookup of a short global like "wK" matches the
// "wK=function" tail of "AwK=function(" or "obj.wK=function(" and returns a
// corrupted definition, masking the correct extractVarInit lookup. The
// "function FN" form is already bounded by the keyword. The boundary character is
// part of the match, so the name offset comes from the capture group, not the
// match start.
func extractFunctionDef(js, name string) (def string, offset int, ok bool) {
	q := regexp.QuoteMeta(name)
	re := regexp.MustCompile(`(?:\bfunction\s+(` + q + `)|(?:^|[^\w$.])(` + q + `)\s*=\s*function)\s*\(`)
	m := re.FindStringSubmatchIndex(js)
	if m == nil {
		return "", 0, false
	}
	start := m[0]
	if m[4] >= 0 { // "FN=function" matched: start at the name, past the boundary char
		start = m[4]
	}
	rel := strings.IndexByte(js[start:], '{')
	if rel < 0 {
		return "", 0, false
	}
	end, ok := matchDelimited(js, start+rel, '{', '}')
	if !ok {
		return "", 0, false
	}
	return js[start:end], start, true
}

// extractVarInit returns "var NAME=<init>" and the offset where NAME begins.
// <init> runs from just after '=' to the first depth-0 ';' or ',' (depth counted
// over (), [], {} and string literals), so a helper object whose body contains
// nested ';' is captured whole. The leading var/let/const is optional in the
// locator so a later declarator (var a=1,NAME=...) still matches; the result is
// always re-emitted with a synthesized "var ". Only a simple assignment counts
// (see isSimpleAssignment), so a comparison or compound-assignment site is not
// mistaken for the declaration.
//
// Known limitation: a regex literal in the initializer (var d=/[{;}]/g) can
// misalign the brace/terminator counting, and matchDelimited does not track
// ${...} interpolation inside template literals. Both are rare in cipher helpers,
// whose globals are strings and arrays; a mislocated snippet fails to parse in
// freeIdentifiers and degrades cleanly. The escalation, if it ever bites, is to
// locate definitions from a parsed AST too.
func extractVarInit(js, name string) (def string, offset int, ok bool) {
	re := regexp.MustCompile(`(?:^|[^\w$.])` + regexp.QuoteMeta(name) + `\s*=`)
	for _, loc := range re.FindAllStringIndex(js, -1) {
		eq := loc[1] - 1 // index of '='
		if !isSimpleAssignment(js, eq) {
			continue
		}
		init := strings.TrimSpace(js[eq+1 : depthZeroEnd(js, eq+1)])
		if init == "" {
			continue
		}
		nameStart := strings.Index(js[loc[0]:loc[1]], name) + loc[0]
		return "var " + name + "=" + init, nameStart, true
	}
	return "", 0, false
}

// isSimpleAssignment reports whether the '=' at eqIdx is a plain assignment, not
// a comparison (== === !=), an arrow (=>), or a compound assignment (+= <<= ??=
// and the rest). A compound operator or comparison puts an operator character
// immediately before '=', and arrows/equality put '=' or '>' immediately after.
func isSimpleAssignment(js string, eqIdx int) bool {
	if eqIdx <= 0 || eqIdx >= len(js)-1 || js[eqIdx] != '=' {
		return false
	}
	if strings.IndexByte("!<>=+-*/%&|^?", js[eqIdx-1]) >= 0 {
		return false
	}
	if c := js[eqIdx+1]; c == '=' || c == '>' {
		return false
	}
	return true
}

// depthZeroEnd returns the index of the first ';' or ',' at or after start that
// sits at bracket depth zero, tracking (), [], {} and string literals so
// separators nested in an initializer do not end it early.
func depthZeroEnd(s string, start int) int {
	depth := 0
	var strCh byte
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if strCh != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == strCh:
				strCh = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			strCh = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ';', ',':
			if depth == 0 {
				return i
			}
		}
	}
	return len(s)
}

// builtinGlobals are global objects and functions that never have a base.js
// top-level definition to bundle, so the closure walker skips scanning for them.
// This is purely an optimization: an unlisted builtin is dropped anyway when
// findTopLevelDefinition finds no definition. Keywords never reach here: goja's
// parser excludes them from the free-identifier set.
var builtinGlobals = map[string]struct{}{
	"Array": {}, "ArrayBuffer": {}, "BigInt": {}, "Boolean": {}, "DataView": {},
	"Date": {}, "Error": {}, "EvalError": {}, "Function": {}, "Infinity": {},
	"JSON": {}, "Map": {}, "Math": {}, "NaN": {}, "Number": {}, "Object": {},
	"Promise": {}, "Proxy": {}, "RangeError": {}, "ReferenceError": {}, "Reflect": {},
	"RegExp": {}, "Set": {}, "String": {}, "Symbol": {}, "SyntaxError": {},
	"TypeError": {}, "URIError": {}, "Uint8Array": {}, "WeakMap": {}, "WeakSet": {},
	"atob": {}, "btoa": {}, "console": {}, "decodeURI": {}, "decodeURIComponent": {},
	"document": {}, "encodeURI": {}, "encodeURIComponent": {}, "escape": {}, "eval": {},
	"globalThis": {}, "isFinite": {}, "isNaN": {}, "location": {}, "navigator": {},
	"parseFloat": {}, "parseInt": {}, "self": {}, "top": {}, "undefined": {},
	"unescape": {}, "window": {},
}

func isReservedOrBuiltin(id string) bool {
	_, ok := builtinGlobals[id]
	return ok
}

// resolveArrayName maps an indirect reference ARR[idx] back to the function name
// it holds, by locating "var ARR=[...]" and reading the idx-th element. Only
// identifier elements are supported; an inline function element is left for a
// future locator pattern.
func resolveArrayName(js, arr string, idx int) (string, bool) {
	loc := regexp.MustCompile(`var\s+` + regexp.QuoteMeta(arr) + `\s*=\s*\[`).FindStringIndex(js)
	if loc == nil {
		return "", false
	}
	rel := strings.IndexByte(js[loc[0]:], '[')
	if rel < 0 {
		return "", false
	}
	end, ok := matchDelimited(js, loc[0]+rel, '[', ']')
	if !ok {
		return "", false
	}
	parts := splitTopLevel(js[loc[0]+rel+1:end-1], ',')
	if idx < 0 || idx >= len(parts) {
		return "", false
	}
	elem := strings.TrimSpace(parts[idx])
	if !identifierRe.MatchString(elem) {
		return "", false
	}
	return elem, true
}

// matchDelimited returns the index just past the delimiter that balances openIdx.
// String literals are tracked so delimiters inside strings do not affect depth.
func matchDelimited(s string, openIdx int, open, close byte) (int, bool) {
	depth := 0
	var strCh byte
	escaped := false
	for i := openIdx; i < len(s); i++ {
		c := s[i]
		if strCh != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == strCh:
				strCh = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			strCh = c
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// splitTopLevel splits s on sep, ignoring separators nested inside (), [], {} or
// string literals.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth := 0
	var strCh byte
	escaped := false
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strCh != 0 {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == strCh:
				strCh = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			strCh = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}
