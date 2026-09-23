package youtube

import (
	"encoding/base64"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
)

// Real player-response xtags values, from an independent encoder.
const (
	xtagsOriginalEn   = "ChEKBWFjb250EghvcmlnaW5hbAoKCgRsYW5nEgJlbg"              // acont=original, lang=en
	xtagsDubbedAutoDe = "ChQKBWFjb250EgtkdWJiZWQtYXV0bwoKCgRsYW5nEgJkZQ"          // acont=dubbed-auto, lang=de
	xtagsOriginalDRC  = "ChEKBWFjb250EghvcmlnaW5hbAoICgNkcmMSATEKCgoEbGFuZxICZW4" // acont=original, drc=1, lang=en
	xtagsOriginalVB   = "ChEKBWFjb250EghvcmlnaW5hbAoKCgRsYW5nEgJlbgoHCgJ2YhIBMQ"  // acont=original, lang=en, vb=1; starts with xtagsOriginalEn
	xtagsDRCOnly      = "CggKA2RyYxIBMQ"                                          // drc=1
	xtagsVB           = "CgcKAnZiEgEx"                                            // vb=1
	xtagsTextForm     = "acont=original:lang=en"                                  // the URL spelling, never in a player response
)

// xtagsPairWire encodes one KeyValuePair {key = 1; value = 2}.
func xtagsPairWire(key, value string) []byte {
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	b = protowire.AppendString(b, key)
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	return protowire.AppendString(b, value)
}

// xtagsWire encodes alternating key, value strings as XTags {repeated
// KeyValuePair xtags = 1}.
func xtagsWire(kv ...string) []byte {
	var b []byte
	for i := 0; i+1 < len(kv); i += 2 {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendBytes(b, xtagsPairWire(kv[i], kv[i+1]))
	}
	return b
}

// xtagsOf spells xtagsWire as the player response does, unpadded base64url.
func xtagsOf(kv ...string) string {
	return base64.RawURLEncoding.EncodeToString(xtagsWire(kv...))
}

func TestXTagsAudioContent(t *testing.T) {
	// The builder must match the real-world DRC sample, or every value built
	// with it tests a layout YouTube does not send.
	if got := xtagsOf("drc", "1"); got != xtagsDRCOnly {
		t.Fatalf("xtagsOf(drc=1) = %q, want %q", got, xtagsDRCOnly)
	}
	if got := xtagsOf("acont", "original", "lang", "en", "vb", "1"); got != xtagsOriginalVB {
		t.Fatalf("xtagsOf(acont=original, lang=en, vb=1) = %q, want %q", got, xtagsOriginalVB)
	}

	unknownTop := protowire.AppendVarint(protowire.AppendTag(nil, 2, protowire.VarintType), 7)
	unknownTop = append(unknownTop, xtagsWire("acont", "original")...)
	unknownInner := protowire.AppendVarint(protowire.AppendTag(nil, 3, protowire.VarintType), 7)
	unknownInner = append(unknownInner, xtagsPairWire("acont", "dubbed")...)
	unknownInner = protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), unknownInner)
	full := xtagsWire("acont", "original", "lang", "en")
	raw := base64.RawURLEncoding.EncodeToString

	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"original", xtagsOriginalEn, "original", true},
		{"dubbed-auto", xtagsDubbedAutoDe, "dubbed-auto", true},
		{"DRC variant keeps its role", xtagsOriginalDRC, "original", true},
		{"vb variant keeps its role", xtagsOriginalVB, "original", true},
		{"drc only", xtagsDRCOnly, "", false},
		{"vb only", xtagsVB, "", false},
		{"empty", "", "", false},
		{"padded", base64.URLEncoding.EncodeToString(full), "original", true},
		{"text form", xtagsTextForm, "", false},
		// A malformed message says nothing, even when its acont came first.
		{"truncated after acont", raw(full[:len(full)-3]), "", false},
		{"invalid UTF-8 key", raw(xtagsWire("\xff", "x", "acont", "original")), "", false},
		{"invalid UTF-8 value", raw(xtagsWire("acont", "\xff")), "", false},
		{"unknown top-level field skipped", raw(unknownTop), "original", true},
		{"unknown pair field skipped", raw(unknownInner), "dubbed", true},
		{"repeated, first wins", xtagsOf("acont", "original", "acont", "dubbed"), "original", true},
		{"empty value skipped", xtagsOf("acont", "", "acont", "dubbed"), "dubbed", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := xtagsAudioContent(tc.in)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("xtagsAudioContent(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func FuzzXTagsAudioContent(f *testing.F) {
	for _, s := range []string{xtagsOriginalEn, xtagsDubbedAutoDe, xtagsOriginalDRC, xtagsOriginalVB, xtagsDRCOnly, xtagsVB, xtagsTextForm, ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, ok := xtagsAudioContent(s)
		if ok != (v != "") || !utf8.ValidString(v) {
			t.Errorf("xtagsAudioContent(%q) = (%q, %v), want a valid non-empty value exactly when ok", s, v, ok)
		}
	})
}
