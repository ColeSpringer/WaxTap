package resolver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/colespringer/waxtap/waxerr"
)

// This file locates the signature and n-parameter transform functions inside
// YouTube's base.js. The transforms themselves are not reimplemented; their
// source is extracted and run in goja. In practice, maintenance is mostly adding
// locator patterns when YouTube changes the surrounding JavaScript.

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

// helperCallRe finds a helper-object call such as OBJ.method(a,N) inside the
// signature function body. The comma requirement skips calls like split("") and
// join("").
var helperCallRe = regexp.MustCompile(`([a-zA-Z0-9$_]+)\.[a-zA-Z0-9$_]+\(\s*[a-zA-Z0-9$_]+\s*,`)

// stsPatterns locate the signature timestamp in base.js. Current players use
// signatureTimestamp:<int>; older players may use sts:<int>. The short-key
// pattern requires an object key so it does not match member assignments such
// as foo.sts=1.
var stsPatterns = []*regexp.Regexp{
	regexp.MustCompile(`signatureTimestamp\s*[:=]\s*(\d+)`),
	regexp.MustCompile(`[{,]\s*"?sts"?\s*:\s*(\d+)`),
}

var identifierRe = regexp.MustCompile(`^[a-zA-Z_$][a-zA-Z0-9_$]*$`)

// extractSignatureSource returns a self-contained JavaScript snippet that defines
// the signature transform as a global function, plus the name to call. The
// snippet bundles the helper object the function relies on, if any. It runs as-is
// in a goja runtime (see jsvm.go).
func extractSignatureSource(js string) (src, name string, err error) {
	name, ok := locateName(signatureNamePatterns, js)
	if !ok {
		return "", "", fmt.Errorf("%w: signature function name not found", waxerr.ErrCipherSolve)
	}
	fnDef, ok := extractFunctionDef(js, name)
	if !ok {
		return "", "", fmt.Errorf("%w: signature function %q body not found", waxerr.ErrCipherSolve, name)
	}

	var b strings.Builder
	if helper, ok := extractHelperObject(js, fnDef); ok {
		b.WriteString(helper)
		b.WriteString(";\n")
	}
	b.WriteString(fnDef)
	b.WriteString(";\n")
	return b.String(), name, nil
}

// extractNSource returns a self-contained snippet defining the n-parameter
// transform as a global function, plus the name to call.
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
	fnDef, ok := extractFunctionDef(js, name)
	if !ok {
		return "", "", fmt.Errorf("%w: n-function %q body not found", waxerr.ErrCipherSolve, name)
	}
	return fnDef + ";\n", name, nil
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

// extractFunctionDef returns the named function definition. Forms accepted here
// are "function FN(...){...}" and "FN=function(...){...}".
func extractFunctionDef(js, name string) (string, bool) {
	q := regexp.QuoteMeta(name)
	loc := regexp.MustCompile(`(?:function\s+` + q + `|` + q + `\s*=\s*function)\s*\(`).FindStringIndex(js)
	if loc == nil {
		return "", false
	}
	rel := strings.IndexByte(js[loc[0]:], '{')
	if rel < 0 {
		return "", false
	}
	end, ok := matchDelimited(js, loc[0]+rel, '{', '}')
	if !ok {
		return "", false
	}
	return js[loc[0]:end], true
}

// extractHelperObject finds the helper object a signature function calls into and
// returns its definition ("OBJ={...}" or "var OBJ={...}"). It returns false when
// the function is self-contained (inline operations, no helper).
func extractHelperObject(js, fnDef string) (string, bool) {
	m := helperCallRe.FindStringSubmatch(fnDef)
	if m == nil {
		return "", false
	}
	loc := regexp.MustCompile(`(?:var\s+)?` + regexp.QuoteMeta(m[1]) + `\s*=\s*\{`).FindStringIndex(js)
	if loc == nil {
		return "", false
	}
	rel := strings.IndexByte(js[loc[0]:], '{')
	if rel < 0 {
		return "", false
	}
	end, ok := matchDelimited(js, loc[0]+rel, '{', '}')
	if !ok {
		return "", false
	}
	return js[loc[0]:end], true
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
