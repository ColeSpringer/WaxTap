package resolver

import "testing"

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
