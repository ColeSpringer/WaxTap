package format

import (
	"errors"
	"testing"
)

// aud builds an audio format with an audio/* MIME type and an average bitrate;
// other fields default to their zero (Unknown) values.
func aud(itag int, codec string, avgBitrate int) Format {
	return Format{Itag: itag, MIMEType: "audio/x", Codec: codec, AverageBitrate: avgBitrate}
}

func TestIsAudio(t *testing.T) {
	cases := []struct {
		mime string
		want bool
	}{
		{"audio/webm; codecs=\"opus\"", true},
		{"audio/mp4", true},
		{"video/mp4; codecs=\"avc1.4d401f\"", false},
		{"", false},
	}
	for _, c := range cases {
		if got := (Format{MIMEType: c.mime}).IsAudio(); got != c.want {
			t.Errorf("IsAudio(%q) = %v, want %v", c.mime, got, c.want)
		}
	}
}

func TestBestAudio_HighestEffectiveBitrate(t *testing.T) {
	// Single-track video: original/DRC unknown for all, so bitrate decides.
	cands := []Format{
		aud(249, "opus", 50000),
		aud(251, "opus", 160000),
		aud(140, "mp4a.40.2", 128000),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("BestAudio idx = %d (itag %d), want 1 (itag 251)", idx, cands[idx].Itag)
	}
}

func TestBestAudio_EffectiveBitratePrefersAverage(t *testing.T) {
	// A high declared peak but low average must lose to a steady higher average.
	cands := []Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", Bitrate: 999000, AverageBitrate: 90000},
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", Bitrate: 130000, AverageBitrate: 128000},
	}
	idx, err := BestAudio().Select(cands, SourcePolicy{}, Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d (itag %d), want 1 (itag 140, higher average)", idx, cands[idx].Itag)
	}
}

func TestBestAudio_PrefersOriginalOverHigherBitrateDub(t *testing.T) {
	cands := []Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, IsOriginal: No},      // dub, higher bitrate
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000, IsOriginal: Yes}, // original
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d (itag %d), want 1 (original track) even at lower bitrate", idx, cands[idx].Itag)
	}
}

func TestBestAudio_PrefersNonDRC(t *testing.T) {
	cands := []Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, IsDRC: Yes}, // compressed, higher bitrate
		{Itag: 250, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 140000, IsDRC: No},  // full dynamic range
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (non-DRC) even at lower bitrate", idx)
	}
}

func TestBestAudio_OriginalOutranksNonDRC(t *testing.T) {
	// Original-but-DRC must beat a non-DRC dub: language correctness first.
	cands := []Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, IsOriginal: No, IsDRC: No},
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000, IsOriginal: Yes, IsDRC: Yes},
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (original track outranks non-DRC dub)", idx)
	}
}

func TestBestAudio_SkipsVideoOnly(t *testing.T) {
	cands := []Format{
		{Itag: 137, MIMEType: "video/mp4", Codec: "avc1.640028", AverageBitrate: 5000000},
		aud(140, "mp4a.40.2", 128000),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (audio); video-only must be skipped", idx)
	}
}

func TestBestAudio_TieBreaksToEarliestIndex(t *testing.T) {
	cands := []Format{aud(251, "opus", 160000), aud(251, "opus", 160000)}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0 (earliest index on a tie)", idx)
	}
}

func TestEligibleAudio_FallbackWhenNoneLabeled(t *testing.T) {
	// No format carries an audio/* MIME type, so all are eligible and bitrate wins.
	cands := []Format{
		{Itag: 1, Codec: "opus", AverageBitrate: 90000},
		{Itag: 2, Codec: "mp4a.40.2", AverageBitrate: 128000},
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1; unlabeled formats must still be selectable", idx)
	}
}

func TestSelect_VideoOnlyCandidatesNoMatch(t *testing.T) {
	// An audio-only selector must reject a list of only video/* renditions rather
	// than fall back to accepting a video stream.
	cands := []Format{
		{Itag: 137, MIMEType: "video/mp4", Codec: "avc1.640028", AverageBitrate: 5000000},
		{Itag: 248, MIMEType: "video/webm", Codec: "vp9", AverageBitrate: 2500000},
	}
	if _, err := BestForTarget(cands, MinimizeLoss(), Target{}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("BestForTarget on video-only: err = %v, want ErrNoMatch", err)
	}
	if _, err := BestAudio().Select(cands, SourcePolicy{}, Target{}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("Select on video-only: err = %v, want ErrNoMatch", err)
	}
}

func TestEligibleAudio_ExcludesVideoInUnlabeledFallback(t *testing.T) {
	// No audio/* candidate, so the fallback applies; it must still exclude the
	// explicit video/* stream and choose the unlabeled audio.
	cands := []Format{
		{Itag: 137, MIMEType: "video/mp4", AverageBitrate: 5000000},
		{Itag: 99, Codec: "opus", AverageBitrate: 128000}, // unlabeled (no MIME)
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (unlabeled audio; video must be excluded)", idx)
	}
}

func TestSelect_Itag(t *testing.T) {
	cands := []Format{aud(251, "opus", 160000), aud(140, "mp4a.40.2", 128000)}
	idx, err := Itag(140).Select(cands, SourcePolicy{}, Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (itag 140)", idx)
	}
}

func TestSelect_ItagRepeatedPicksOriginal(t *testing.T) {
	// itag is not unique across language tracks; the original must win.
	cands := []Format{
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000, IsOriginal: No},
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000, IsOriginal: Yes},
	}
	idx, err := Itag(140).Select(cands, SourcePolicy{}, Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (original among repeated itag 140)", idx)
	}
}

func TestSelect_ItagNotFound(t *testing.T) {
	cands := []Format{aud(251, "opus", 160000)}
	if _, err := Itag(999).Select(cands, SourcePolicy{}, Target{}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
}

func TestSelect_Codec(t *testing.T) {
	cands := []Format{aud(251, "opus", 160000), aud(140, "mp4a.40.2", 128000)}
	// "aac" must match the normalized "mp4a.40.2" codec id.
	idx, err := Codec("aac").Select(cands, SourcePolicy{}, Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (aac family matches mp4a.40.2)", idx)
	}
}

func TestSelect_CodecPicksBestOfFamily(t *testing.T) {
	cands := []Format{aud(249, "opus", 50000), aud(140, "mp4a.40.2", 128000), aud(251, "opus", 160000)}
	idx, err := Codec("opus").Select(cands, SourcePolicy{}, Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 2 {
		t.Fatalf("idx = %d, want 2 (highest-bitrate opus)", idx)
	}
}

func TestSelect_CodecNotFound(t *testing.T) {
	cands := []Format{aud(251, "opus", 160000)}
	if _, err := Codec("vorbis").Select(cands, SourcePolicy{}, Target{}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
}

func TestBestForTarget_MinimizeLossMatchesTargetCodec(t *testing.T) {
	// Opus is higher bitrate, but an AAC target should start from the AAC source
	// to avoid a cross-codec transcode.
	cands := []Format{aud(251, "opus", 160000), aud(140, "mp4a.40.2", 128000)}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{Codec: "aac"})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (AAC source for AAC target)", idx)
	}
}

func TestBestForTarget_MinimizeLossFallsBackWhenNoCodecMatch(t *testing.T) {
	// No AAC source present: MinimizeLoss falls back to the best available audio.
	cands := []Format{aud(251, "opus", 160000), aud(250, "vorbis", 128000)}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{Codec: "aac"})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0 (best source when no codec match)", idx)
	}
}

func TestBestForTarget_OriginalOutranksCodecPreference(t *testing.T) {
	// The AAC stream is a dub; the original track is Opus. A codec preference must
	// not trade the correct language for a codec match.
	cands := []Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, IsOriginal: Yes},    // original
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000, IsOriginal: No}, // dub
	}
	if idx, err := BestForTarget(cands, MinimizeLoss(), Target{Codec: "aac"}); err != nil || idx != 0 {
		t.Fatalf("MinimizeLoss idx=%d err=%v, want 0 (original Opus, not the AAC dub)", idx, err)
	}
	if idx, err := BestForTarget(cands, PreferCodec("aac"), Target{Codec: "mp3"}); err != nil || idx != 0 {
		t.Fatalf("PreferCodec idx=%d err=%v, want 0 (original Opus, not the AAC dub)", idx, err)
	}
}

func TestBestForTarget_CodecBreaksTieAmongOriginals(t *testing.T) {
	// Both tracks are original: the codec preference then selects the AAC source
	// even though the Opus source has a higher bitrate (codec outranks bitrate).
	cands := []Format{
		{Itag: 251, MIMEType: "audio/webm", Codec: "opus", AverageBitrate: 160000, IsOriginal: Yes},
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000, IsOriginal: Yes},
	}
	if idx, err := BestForTarget(cands, MinimizeLoss(), Target{Codec: "aac"}); err != nil || idx != 1 {
		t.Fatalf("idx=%d err=%v, want 1 (AAC original for AAC target)", idx, err)
	}
}

func TestBestForTarget_BestNativeIgnoresTargetCodec(t *testing.T) {
	cands := []Format{aud(251, "opus", 160000), aud(140, "mp4a.40.2", 128000)}
	idx, err := BestForTarget(cands, BestNative(), Target{Codec: "aac"})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0 (BestNative ignores target codec, takes highest bitrate)", idx)
	}
}

func TestBestForTarget_PreferCodec(t *testing.T) {
	cands := []Format{aud(251, "opus", 160000), aud(140, "mp4a.40.2", 128000)}
	// A lossy (non-lossless, non-empty) target activates the policy; PreferCodec
	// uses its own codec, not the target's.
	idx, err := BestForTarget(cands, PreferCodec("aac"), Target{Codec: "mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (PreferCodec aac)", idx)
	}
}

func TestBestForTarget_LosslessIgnoresPolicy(t *testing.T) {
	// FLAC/ALAC/WAV: every policy picks the best source, ignoring codec matching.
	cands := []Format{aud(251, "opus", 160000), aud(140, "mp4a.40.2", 128000)}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{Lossless: true})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0 (lossless target takes best source, not codec-matched)", idx)
	}
}

func TestBestForTarget_NoTargetIsBestAudio(t *testing.T) {
	cands := []Format{aud(140, "mp4a.40.2", 128000), aud(251, "opus", 160000)}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (no transcode -> best audio)", idx)
	}
}

func TestSelect_BestAudioDelegatesToTarget(t *testing.T) {
	// The zero/BestAudio selector must pass policy and target to BestForTarget.
	cands := []Format{aud(251, "opus", 160000), aud(140, "mp4a.40.2", 128000)}
	idx, err := BestAudio().Select(cands, MinimizeLoss(), Target{Codec: "aac"})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("idx = %d, want 1 (BestAudio honors MinimizeLoss + AAC target)", idx)
	}
	// And the zero AudioSelector behaves identically to BestAudio().
	var zero AudioSelector
	got, err := zero.Select(cands, MinimizeLoss(), Target{Codec: "aac"})
	if err != nil {
		t.Fatal(err)
	}
	if got != idx {
		t.Fatalf("zero selector idx = %d, want %d", got, idx)
	}
}

func TestSelect_EmptyCandidates(t *testing.T) {
	if _, err := BestAudio().Select(nil, SourcePolicy{}, Target{}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("BestAudio on empty: err = %v, want ErrNoMatch", err)
	}
	if _, err := BestForTarget(nil, MinimizeLoss(), Target{}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("BestForTarget on empty: err = %v, want ErrNoMatch", err)
	}
}

func TestCodecMatches(t *testing.T) {
	cases := []struct {
		want, have string
		match      bool
	}{
		{"aac", "mp4a.40.2", true},
		{"aac", "mp4a.40.5", true},
		{"m4a", "mp4a.40.2", true},
		{"opus", "opus", true},
		{"vorbis", "vorbis", true},
		{"aac", "opus", false},
		{"opus", "mp4a.40.2", false},
		{"", "opus", false}, // empty want never matches
		{"weirdcodec", "weirdcodec", true},
	}
	for _, c := range cases {
		if got := codecMatches(c.want, c.have); got != c.match {
			t.Errorf("codecMatches(%q, %q) = %v, want %v", c.want, c.have, got, c.match)
		}
	}
}
