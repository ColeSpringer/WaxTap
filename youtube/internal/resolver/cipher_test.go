package resolver

import (
	"os"
	"testing"
)

func TestExtractSignatureTimestamp(t *testing.T) {
	sts, ok := extractSignatureTimestamp(readPlayerSynth(t))
	if !ok {
		t.Fatal("signature timestamp not found in player_synth.js fixture")
	}
	if sts != 19834 {
		t.Errorf("signature timestamp = %d, want 19834", sts)
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
		{"signatureTimestamp with space", `var cfg = { signatureTimestamp: 20611, foo: 1 };`, 20611, true},
		{"signatureTimestamp embedded in larger object", `{name:"player",signatureTimestamp:20606,extra:true}`, 20606, true},
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
		// Equals separator inside an object field is tolerated (still anchored to
		// {/, so member assignments stay ignored).
		{"equals separator in object field", `{"name":"x",signatureTimestamp=20607}`, 20607, true},
		{"sts equals separator", `{a:1,sts=16000}`, 16000, true},
		// Scan all matches and return the first positive: a leading zero-valued
		// decoy must not shadow the real value later in the file, and a positive
		// value must win over any decoy that follows it.
		{"leading zero-value decoy skipped", `{signatureTimestamp:00000,signatureTimestamp:20607}`, 20607, true},
		{"first positive wins over a later value", `{signatureTimestamp:20607,signatureTimestamp:19998}`, 20607, true},
		{"trailing sts:0 decoy after real value", `{signatureTimestamp:20611,b:1,sts:0}`, 20611, true},
		{"short sts:0 has no positive match", `{sts:0}`, 0, false},
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

// TestExtractSignatureTimestamp_Builds parses minimal committed snippets of the
// player_es6 and embed-fallback builds. Each carries a real timestamp plus a
// decoy (a leading sts:0 in player_es6, a trailing sts:0 in embed) so both the
// anchor and the full scan are exercised against realistic surrounding shapes.
func TestExtractSignatureTimestamp_Builds(t *testing.T) {
	cases := []struct {
		file string
		want int
	}{
		{"testdata/sts_player_es6.js", 20607},
		{"testdata/sts_embed.js", 20611},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			b, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			sts, ok := extractSignatureTimestamp(string(b))
			if !ok || sts != tc.want {
				t.Errorf("extractSignatureTimestamp = (%d, %v), want (%d, true)", sts, ok, tc.want)
			}
		})
	}
}
