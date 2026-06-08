package resolver

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja/parser"

	"github.com/colespringer/waxtap/waxerr"
)

// readPlayerSynth returns the synthetic whole-player fixture used across the
// resolver tests. No real player JS is committed (licensing); the fixture mirrors
// the shapes the solver targets: an IIFE-wrapped player whose descrambler is
// found by its direct alr/yes statement.
func readPlayerSynth(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/player_synth.js")
	if err != nil {
		t.Fatalf("read player_synth.js: %v", err)
	}
	return string(b)
}

// urlMachinery is a set of URL-like classes the inline descrambler fixtures
// share. Each class exposes set/get/clone plus exactly one extra prototype
// method (the transform the driver invokes): YUrl reverses, IdUrl is a no-op,
// AltUrl reverses-and-appends (a different valid result), LoopUrl spins forever.
const urlMachinery = `
var helper={conf:function(a,b){return a+b}};
function rev(x){return String(x).split("").reverse().join("")}
function YUrl(raw,key,val){this.params={};if(key!==undefined&&val!==undefined)this.params[key]=val}
YUrl.prototype.set=function(k,v){this.params[k]=v};
YUrl.prototype.get=function(k){return this.params[k]};
YUrl.prototype.clone=function(){return this};
YUrl.prototype.descramble=function(){if(this.params.n!==undefined)this.params.n=rev(this.params.n);if(this.params.s!==undefined)this.params.s=rev(this.params.s)};
function IdUrl(raw,key,val){this.params={};if(key!==undefined&&val!==undefined)this.params[key]=val}
IdUrl.prototype.set=function(k,v){this.params[k]=v};
IdUrl.prototype.get=function(k){return this.params[k]};
IdUrl.prototype.clone=function(){return this};
IdUrl.prototype.noop=function(){};
function AltUrl(raw,key,val){this.params={};if(key!==undefined&&val!==undefined)this.params[key]=val}
AltUrl.prototype.set=function(k,v){this.params[k]=v};
AltUrl.prototype.get=function(k){return this.params[k]};
AltUrl.prototype.clone=function(){return this};
AltUrl.prototype.alt=function(){if(this.params.n!==undefined)this.params.n=rev(this.params.n)+"Q"};
function LoopUrl(raw,key,val){this.params={}}
LoopUrl.prototype.set=function(k,v){this.params[k]=v};
LoopUrl.prototype.get=function(k){return this.params[k]};
LoopUrl.prototype.clone=function(){return this};
LoopUrl.prototype.spin=function(){for(;;){}};
`

const (
	descReal  = `function realDesc(raw,key,val){helper.conf("alr","yes");return new YUrl(raw,key,val)}`
	descEcho  = `function echoDesc(raw,key,val){helper.conf("alr","yes");return new IdUrl(raw,key,val)}`
	descAlt   = `function altDesc(raw,key,val){helper.conf("alr","yes");return new AltUrl(raw,key,val)}`
	descThrow = `function throwDesc(raw,key,val){helper.conf("alr","yes");throw new Error("decoy")}`
	descLoop  = `function loopDesc(raw,key,val){helper.conf("alr","yes");return new LoopUrl(raw,key,val)}`
	descViaG  = `g.gDesc=function(raw,key,val){helper.conf("alr","yes");return new YUrl(raw,key,val)}`
)

// wrapPlayer builds an IIFE-wrapped synthetic player from the shared URL
// machinery plus the given descrambler definitions, matching the real
// var X={};(function(g){…})(X) shape the solver unwraps.
func wrapPlayer(descramblers ...string) string {
	return "var T={};(function(g){" + urlMachinery + strings.Join(descramblers, "\n") + "\n})(T);"
}

func TestParsePlayer_Shapes(t *testing.T) {
	cases := []struct {
		name    string
		js      string
		wantArg string
	}{
		{"two-statement var+call", `var X={};(function(g){var a=1})(X);`, "g"},
		{"call(this) one-statement", `(function(){var a=1}).call(this);`, ""}, // parameterless: no namespace
		{"named first parameter", `var X={};(function(ns){var a=1})(X);`, "ns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iife, argName, err := parsePlayer(tc.js)
			if err != nil {
				t.Fatalf("parsePlayer: %v", err)
			}
			if iife == nil {
				t.Fatal("iife is nil")
			}
			if argName != tc.wantArg {
				t.Errorf("argName = %q, want %q", argName, tc.wantArg)
			}
		})
	}
}

func TestParsePlayer_Malformed(t *testing.T) {
	cases := []struct {
		name string
		js   string
	}{
		{"no IIFE", `var x = 1; function f(){ return 2 }`},
		{"parse error", `var x = ;`},
		{"plain call, not a function", `var x={};foo(x);`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := parsePlayer(tc.js); !errors.Is(err, waxerr.ErrCipherSolve) {
				t.Fatalf("err = %v, want ErrCipherSolve", err)
			}
		})
	}
}

// TestUnwrapPlayer hoists the IIFE body to global scope: the result starts with
// the fresh namespace object, carries the inner definitions, and compiles. A
// 'use strict' directive that was inside the IIFE must not make the hoisted
// global program strict (it is no longer in directive position).
func TestUnwrapPlayer(t *testing.T) {
	js := `var X={};(function(g){'use strict';function f(){return 1}g.f=f})(X);`
	iife, argName, err := parsePlayer(js)
	if err != nil {
		t.Fatal(err)
	}
	src, err := unwrapPlayer(js, iife, argName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(src, "var g={};\n") {
		t.Errorf("unwrapped source missing namespace prefix:\n%s", src)
	}
	if !strings.Contains(src, "function f()") {
		t.Errorf("unwrapped source dropped the inner definition:\n%s", src)
	}
	if _, err := goja.Compile("unwrapped", src, false); err != nil {
		t.Fatalf("unwrapped source does not compile: %v", err)
	}
}

// TestFindDescramblers_Forms checks every definition form is recognized and gets
// a reference that resolves after the player runs: a bare name for function decl,
// var/let/const initializer, and bare assignment (globalThis[name] would miss a
// let/const), and a member expression for the obj.X form. A nested alr/yes call
// is NOT matched.
func TestFindDescramblers_Forms(t *testing.T) {
	js := `var T={};(function(g){` +
		`var h={conf:function(a,b){return a}};` +
		`function fnDecl(){h.conf("alr","yes");return 0}` +
		`var varForm=function(){h.conf("alr","yes");return 0};` +
		`let letForm=function(){h.conf("alr","yes");return 0};` +
		`bareForm=function(){h.conf("alr","yes");return 0};` +
		`g.memberForm=function(){h.conf("alr","yes");return 0};` +
		`function nested(){if(1){h.conf("alr","yes")}return 0}` +
		`})(T);`

	iife, _, err := parsePlayer(js)
	if err != nil {
		t.Fatal(err)
	}
	cands := findDescramblers("test", iife)

	ref := map[string]string{}
	for _, c := range cands {
		ref[c.name] = c.ref
	}
	// Declarations resolve by bare name; the member form by its receiver.
	want := map[string]string{
		"fnDecl": "fnDecl", "varForm": "varForm", "letForm": "letForm",
		"bareForm": "bareForm", "memberForm": "g.memberForm",
	}
	for name, wantRef := range want {
		if ref[name] != wantRef {
			t.Errorf("descrambler %q ref = %q, want %q", name, ref[name], wantRef)
		}
	}
	if _, ok := ref["nested"]; ok {
		t.Error("a nested (non-direct) alr/yes call was matched as a descrambler")
	}
}

// TestFindDescramblers_Synth checks the committed fixture yields exactly the real
// descrambler and the throwing decoy — not the nested or get("n") decoys.
func TestFindDescramblers_Synth(t *testing.T) {
	iife, _, err := parsePlayer(readPlayerSynth(t))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range findDescramblers("synth", iife) {
		got[c.name] = true
	}
	if !got["realDesc"] {
		t.Errorf("realDesc not found; got %v", got)
	}
	for _, decoy := range []string{"nestedDecoy", "getNDecoy"} {
		if got[decoy] {
			t.Errorf("decoy %q wrongly matched as a descrambler", decoy)
		}
	}
}

// TestSolveGolden runs both transforms against the committed fixture end to end:
// parse, unwrap, compile, discover, drive, consensus.
func TestSolveGolden(t *testing.T) {
	p := compilePlayerProgram("https://www.youtube.com/s/player/synth/base.js", readPlayerSynth(t))
	if p.compileErr != nil {
		t.Fatalf("compile failed: %v", p.compileErr)
	}
	gotSig, err := p.decipherSignature(context.Background(), "ABCDEFGH", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if gotSig != "HGFEDCBA" {
		t.Errorf("decipherSignature(ABCDEFGH) = %q, want HGFEDCBA", gotSig)
	}
	gotN, err := p.decodeN(context.Background(), "12345", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if gotN != "54321" {
		t.Errorf("decodeN(12345) = %q, want 54321", gotN)
	}
}

// TestSolveSession checks one session (one player execution) solves both the
// signature and n transforms, the path Resolve uses to avoid loading the player
// twice per ciphered video.
func TestSolveSession(t *testing.T) {
	p := compilePlayerProgram("u", readPlayerSynth(t))
	if p.compileErr != nil {
		t.Fatal(p.compileErr)
	}
	sess, err := p.openSession(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.close()

	sig, err := sess.solve(kindSig, "ABCDEFGH")
	if err != nil {
		t.Fatal(err)
	}
	if sig != "HGFEDCBA" {
		t.Errorf("session sig = %q, want HGFEDCBA", sig)
	}
	n, err := sess.solve(kindN, "12345")
	if err != nil {
		t.Fatal(err)
	}
	if n != "54321" {
		t.Errorf("session n = %q, want 54321", n)
	}
}

// TestSolveConsensus covers the pre-filter + consensus behavior that guards
// against Bug A (a false positive that runs without throwing).
func TestSolveConsensus(t *testing.T) {
	ctx := context.Background()

	t.Run("echo and throw filtered, real wins", func(t *testing.T) {
		p := compilePlayerProgram("u", wrapPlayer(descReal, descEcho, descThrow))
		if p.compileErr != nil {
			t.Fatal(p.compileErr)
		}
		got, err := p.decodeN(ctx, "12345", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got != "54321" {
			t.Errorf("decodeN = %q, want 54321 (decoys filtered)", got)
		}
	})

	t.Run("signature echo decoy is filtered", func(t *testing.T) {
		// An echo-shaped decoy returns the input signature unchanged; without the
		// echo filter on the signature path it would break consensus (two distinct
		// valid results) even though the n path tolerates it.
		p := compilePlayerProgram("u", wrapPlayer(descReal, descEcho))
		if p.compileErr != nil {
			t.Fatal(p.compileErr)
		}
		got, err := p.decipherSignature(ctx, "ABCDEFGH", time.Second)
		if err != nil {
			t.Fatalf("signature consensus broke on a benign echo decoy: %v", err)
		}
		if got != "HGFEDCBA" {
			t.Errorf("decipherSignature = %q, want HGFEDCBA (echo filtered)", got)
		}
	})

	t.Run("two disagreeing valid results fail closed", func(t *testing.T) {
		p := compilePlayerProgram("u", wrapPlayer(descReal, descAlt))
		if p.compileErr != nil {
			t.Fatal(p.compileErr)
		}
		_, err := p.decodeN(ctx, "12345", time.Second)
		if !errors.Is(err, waxerr.ErrCipherSolve) {
			t.Fatalf("err = %v, want ErrCipherSolve for disagreeing candidates", err)
		}
	})

	t.Run("no valid result errors, never returns undefined", func(t *testing.T) {
		p := compilePlayerProgram("https://www.youtube.com/s/player/zz/base.js", wrapPlayer(descEcho))
		if p.compileErr != nil {
			t.Fatal(p.compileErr)
		}
		got, err := p.decodeN(ctx, "12345", time.Second)
		if !errors.Is(err, waxerr.ErrCipherSolve) {
			t.Fatalf("err = %v, want ErrCipherSolve", err)
		}
		if got == "undefined" {
			t.Error("solve returned the string \"undefined\" instead of an error (Bug A regression)")
		}
		if !strings.Contains(err.Error(), "player/zz") {
			t.Errorf("error should name the player URL for observability: %v", err)
		}
	})

	t.Run("g.X member descrambler solves", func(t *testing.T) {
		p := compilePlayerProgram("u", wrapPlayer(descViaG))
		if p.compileErr != nil {
			t.Fatal(p.compileErr)
		}
		got, err := p.decodeN(ctx, "12345", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got != "54321" {
			t.Errorf("decodeN via g.X = %q, want 54321", got)
		}
	})

	t.Run("const descrambler solves", func(t *testing.T) {
		// const/let bindings never attach to the global object, so the reference
		// must be the bare name, not globalThis["..."].
		p := compilePlayerProgram("u", wrapPlayer(`const constDesc=function(raw,key,val){helper.conf("alr","yes");return new YUrl(raw,key,val)}`))
		if p.compileErr != nil {
			t.Fatal(p.compileErr)
		}
		got, err := p.decodeN(ctx, "12345", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got != "54321" {
			t.Errorf("decodeN via const = %q, want 54321", got)
		}
	})

	t.Run("this.X descrambler in call(this) player solves", func(t *testing.T) {
		// A parameterless .call(this) player attaches the descrambler to `this`
		// (the global object at top level); the reference must be globalThis.X.
		js := "(function(){" + urlMachinery +
			`this.tDesc=function(raw,key,val){helper.conf("alr","yes");return new YUrl(raw,key,val)}` +
			"\n}).call(this);"
		p := compilePlayerProgram("u", js)
		if p.compileErr != nil {
			t.Fatal(p.compileErr)
		}
		got, err := p.decodeN(ctx, "12345", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got != "54321" {
			t.Errorf("decodeN via this.X = %q, want 54321", got)
		}
	})
}

func TestValidResult(t *testing.T) {
	cases := []struct {
		kind transformKind
		in   string
		out  string
		want bool
	}{
		{kindN, "12345", "", false},
		{kindN, "12345", "undefined", false},
		{kindN, "12345", "12345", false},                   // echo
		{kindN, "12345", "enhanced_except_a_12345", false}, // YouTube failure sentinel (suffix)
		{kindN, "12345", "54321", true},
		{kindSig, "ABC", "", false},
		{kindSig, "ABC", "undefined", false},
		{kindSig, "ABC", "ABC", false}, // echo filtered for the signature too
		{kindSig, "ABC", "XABC", true}, // a suffix is fine for sig (n-only sentinel)
		{kindSig, "ABC", "CBA", true},
	}
	for _, tc := range cases {
		if got := validResult(tc.kind, tc.in, tc.out); got != tc.want {
			t.Errorf("validResult(%v, %q, %q) = %v, want %v", tc.kind, tc.in, tc.out, got, tc.want)
		}
	}
}

// TestEnvStub checks the embedded browser stub runs in a fresh runtime and
// defines a representative global.
func TestEnvStub(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunProgram(envStubProgram); err != nil {
		t.Fatalf("env stub failed to run: %v", err)
	}
	// console and a fetch chained through .finally must not throw during load.
	if _, err := vm.RunString(`console.log("x"); fetch("y").then(function(){}).catch(function(){}).finally(function(){})`); err != nil {
		t.Errorf("console/fetch stub is not load-safe: %v", err)
	}
	v, err := vm.RunString(`location.hostname`)
	if err != nil {
		t.Fatalf("read stubbed global: %v", err)
	}
	if v.String() != "www.youtube.com" {
		t.Errorf("location.hostname = %q, want www.youtube.com", v.String())
	}
}

// TestGojaES6Floor documents the modern JS the whole-player pipeline depends on:
// goja's parser must parse it and goja must compile it. A goja downgrade that
// drops any of these fails here rather than silently breaking on a real player.
func TestGojaES6Floor(t *testing.T) {
	const snippet = `
var f=async function(a){const g=(x)=>x??0;let {p=1}=a||{};let arr=[...a];
  const v=await Promise.resolve(1);
  return g(a?.b)+p+` + "`v${p}`" + `+arr.length+v};
function* gen(){yield 1}
class C{#x=1;getX(){return this.#x}}`
	if _, err := parser.ParseFile(nil, "", snippet, parser.IgnoreRegExpErrors); err != nil {
		t.Fatalf("goja parser rejected an ES6+ snippet (parse floor regressed): %v", err)
	}
	if _, err := goja.Compile("es6", snippet, false); err != nil {
		t.Fatalf("goja compiler rejected an ES6+ snippet (compile floor regressed): %v", err)
	}
}
