package waxtap

import (
	"strings"
	"testing"
)

// audFmt builds an audio candidate for source-policy tests.
func audFmt(itag int, codec string, bitrate int) Format {
	return Format{Itag: itag, MIMEType: "audio/webm", Codec: codec, AverageBitrate: bitrate}
}

// A prefer:<codec> that parses but matches nothing on this video is silently
// ineffective today: the delivery looks exactly like a run with no policy at
// all, so the only way to learn the preference did not apply is to compare
// codecs by hand.
func TestWarnUnboundSourcePolicy(t *testing.T) {
	opus := audFmt(251, "opus", 160000)
	aac := audFmt(140, "mp4a.40.2", 128000)

	for _, tc := range []struct {
		name    string
		policy  SourcePolicy
		formats []Format
		chosen  Format
		want    string // "" means no warning
	}{
		{"unmatched", PreferCodec("flac"), []Format{aac, opus}, opus, "aac, opus"},
		{"matched and chosen", PreferCodec("opus"), []Format{aac, opus}, opus, ""},
		{"present but outranked", PreferCodec("aac"), []Format{aac, opus}, opus, ""},
		{"minimize-loss", MinimizeLoss(), []Format{aac, opus}, opus, ""},
		{"best-native", BestNative(), []Format{aac, opus}, opus, ""},
		{"alias spelling matches", PreferCodec("mp4a.40.2"), []Format{aac, opus}, aac, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			em := newEmitter(nil, "dummyVideo0")
			warnUnboundSourcePolicy(em, tc.policy, tc.formats, tc.chosen, "delivering")
			detail := ""
			for _, w := range em.warnings {
				if w.Code == WarnSourcePolicyUnmatched {
					detail = w.Detail
				}
			}
			if tc.want == "" {
				if detail != "" {
					t.Fatalf("unexpected warning: %q", detail)
				}
				return
			}
			if detail == "" {
				t.Fatalf("no %s warning; warnings = %+v", WarnSourcePolicyUnmatched, em.warnings)
			}
			if !strings.Contains(detail, tc.want) {
				t.Errorf("detail %q does not list the available codecs %q", detail, tc.want)
			}
			if !strings.Contains(detail, tc.policy.Preferred()) {
				t.Errorf("detail %q does not name the preference", detail)
			}
			if !strings.Contains(detail, "opus") {
				t.Errorf("detail %q does not name what was delivered", detail)
			}
		})
	}
}

// A video whose formats list is empty (or holds only video streams) must still
// produce a readable sentence rather than a dangling "available codecs: ".
func TestWarnUnboundSourcePolicyNoCandidates(t *testing.T) {
	em := newEmitter(nil, "dummyVideo0")
	warnUnboundSourcePolicy(em, PreferCodec("flac"), nil, audFmt(251, "opus", 160000), "delivering")
	if len(em.warnings) != 1 {
		t.Fatalf("warnings = %+v, want exactly one", em.warnings)
	}
	if d := em.warnings[0].Detail; strings.Contains(d, "codecs: )") || strings.HasSuffix(d, ": ") {
		t.Errorf("empty codec list renders badly: %q", d)
	}
}
