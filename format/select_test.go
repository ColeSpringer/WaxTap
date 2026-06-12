package format

import (
	"errors"
	"fmt"
	"testing"
)

// aud builds an audio format with an audio/* MIME type and an average bitrate;
// other fields default to their zero (Unknown) values.
func aud(itag int, codec string, avgBitrate int) Format {
	return Format{Itag: itag, MIMEType: "audio/x", Codec: codec, AverageBitrate: avgBitrate}
}

// audq builds an audio format with a reported quality tier.
func audq(itag int, codec, ext string, avgBitrate int, tier AudioQualityTier) Format {
	return Format{
		Itag:           itag,
		MIMEType:       "audio/" + ext,
		Codec:          codec,
		Extension:      ext,
		AverageBitrate: avgBitrate,
		AudioQuality:   tier,
	}
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

func TestBestForTarget_PreferCodecHonoredForNoneTarget(t *testing.T) {
	cands := []Format{aud(251, "opus", 160000), aud(140, "mp4a.40.2", 128000)}
	for _, tc := range []struct {
		name   string
		target Target
	}{
		{"none/mp3", Target{}},
		{"lossless/flac", Target{Lossless: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, err := BestForTarget(cands, PreferCodec("aac"), tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if idx != 1 {
				t.Fatalf("idx = %d, want 1 (prefer:aac selects the AAC source over opus)", idx)
			}
		})
	}
}

func TestBestForTarget_NoneTargetDefaultStillBestAudio(t *testing.T) {
	cands := []Format{aud(251, "opus", 160000), aud(140, "mp4a.40.2", 128000)}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Fatalf("idx = %d, want 0 (opus, highest bitrate; default ranking unchanged)", idx)
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

func TestBestAudio_TierThenOpusBeatsHigherBitrate(t *testing.T) {
	cands := []Format{
		audq(251, "opus", "webm", 105000, QualityMedium),
		audq(140, "mp4a.40.2", "m4a", 129000, QualityMedium),
		audq(139, "mp4a.40.5", "m4a", 49000, QualityLow),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 251 {
		t.Fatalf("idx=%d itag=%d, want 251 (Opus MEDIUM over 129k AAC MEDIUM)", idx, cands[idx].Itag)
	}
}

func TestBestAudio_SameTierOpusWinsRegardlessOfBitrate(t *testing.T) {
	cands := []Format{
		audq(251, "opus", "webm", 110000, QualityMedium),
		audq(140, "mp4a.40.2", "m4a", 160000, QualityMedium),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 251 {
		t.Fatalf("idx=%d itag=%d, want 251 (Opus over higher-bitrate AAC in same tier)", idx, cands[idx].Itag)
	}
}

func TestBestAudio_HigherTierBeatsLowerTier(t *testing.T) {
	cands := []Format{
		audq(251, "opus", "webm", 50000, QualityLow),
		audq(140, "mp4a.40.2", "m4a", 128000, QualityMedium),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 140 {
		t.Fatalf("idx=%d itag=%d, want 140 (MEDIUM beats LOW)", idx, cands[idx].Itag)
	}
}

func TestBestAudio_UltraLowBelowLow(t *testing.T) {
	cands := []Format{
		audq(600, "opus", "webm", 60000, QualityUltraLow),
		audq(139, "mp4a.40.5", "m4a", 55000, QualityLow),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 139 {
		t.Fatalf("idx=%d itag=%d, want 139 (LOW AAC beats ULTRALOW Opus)", idx, cands[idx].Itag)
	}
}

func TestBestAudio_HighTierBeatsOpusPreference(t *testing.T) {
	cands := []Format{
		audq(140, "mp4a.40.2", "m4a", 128000, QualityHigh),
		audq(251, "opus", "webm", 160000, QualityMedium),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 140 {
		t.Fatalf("idx=%d itag=%d, want 140 (HIGH tier beats MEDIUM Opus)", idx, cands[idx].Itag)
	}
}

func TestBestAudio_WithinTierSameCodecHigherBitrate(t *testing.T) {
	cands := []Format{
		audq(250, "opus", "webm", 120000, QualityMedium),
		audq(251, "opus", "webm", 160000, QualityMedium),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 251 {
		t.Fatalf("idx=%d itag=%d, want 251 (higher bitrate within same codec/tier)", idx, cands[idx].Itag)
	}
}

func TestBestAudio_MixedTierMetadataFallsBackToBitrate(t *testing.T) {
	cands := []Format{
		audq(251, "opus", "webm", 105000, QualityMedium),
		audq(140, "mp4a.40.2", "m4a", 129000, QualityUnknown),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 140 {
		t.Fatalf("idx=%d itag=%d, want 140 (bitrate fallback when a candidate lacks a tier)", idx, cands[idx].Itag)
	}
}

func TestTierUsable_OnlyHighestPriorityCandidates(t *testing.T) {
	t.Run("dub without tier", func(t *testing.T) {
		cands := []Format{
			{Itag: 251, MIMEType: "audio/webm", Codec: "opus", Extension: "webm", AverageBitrate: 105000, AudioQuality: QualityMedium, IsOriginal: Yes},
			{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", Extension: "m4a", AverageBitrate: 129000, AudioQuality: QualityMedium, IsOriginal: Yes},
			{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", Extension: "m4a", AverageBitrate: 200000, AudioQuality: QualityUnknown, IsOriginal: No},
		}
		idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
		if err != nil {
			t.Fatal(err)
		}
		if cands[idx].Itag != 251 || cands[idx].Codec != "opus" {
			t.Fatalf("idx=%d itag=%d codec=%s, want 251 opus", idx, cands[idx].Itag, cands[idx].Codec)
		}
	})

	t.Run("excluded video without tier", func(t *testing.T) {
		cands := []Format{
			audq(251, "opus", "webm", 105000, QualityMedium),
			audq(140, "mp4a.40.2", "m4a", 129000, QualityMedium),
			{Itag: 137, MIMEType: "video/mp4", Codec: "avc1.640028", AverageBitrate: 4000000},
		}
		idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
		if err != nil {
			t.Fatal(err)
		}
		if cands[idx].Itag != 251 {
			t.Fatalf("idx=%d itag=%d, want 251 (video-only exclusion must not disable tier)", idx, cands[idx].Itag)
		}
	})
}

func TestBestForTarget_PreferredCodecDominatesTier(t *testing.T) {
	cands := []Format{
		audq(140, "mp4a.40.2", "m4a", 100000, QualityLow),
		audq(251, "opus", "webm", 200000, QualityHigh),
	}
	idx, err := BestForTarget(cands, PreferCodec("aac"), Target{Codec: "mp3"})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 140 {
		t.Fatalf("idx=%d itag=%d, want 140 (preferred codec dominates tier)", idx, cands[idx].Itag)
	}
}

func TestBestForTarget_TierFallbackAmongNonPreferred(t *testing.T) {
	cands := []Format{
		audq(251, "opus", "webm", 105000, QualityMedium),
		audq(140, "mp4a.40.2", "m4a", 129000, QualityLow),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{Codec: "vorbis"})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 251 {
		t.Fatalf("idx=%d itag=%d, want 251 (MEDIUM Opus over LOW AAC when no codec match)", idx, cands[idx].Itag)
	}
}

func TestCodecPreferenceRank(t *testing.T) {
	cases := []struct {
		codec string
		want  int
	}{
		{"opus", 1},
		{"aac", 0},
		{"mp4a.40.2", 0},
		{"vorbis", 0},
		{"mp3", 0},
		{"", 0},
		{"weirdcodec", 0},
	}
	for _, c := range cases {
		if got := codecPreferenceRank(c.codec); got != c.want {
			t.Errorf("codecPreferenceRank(%q) = %d, want %d", c.codec, got, c.want)
		}
	}
}

func TestBestAudio_PicksWebmOpusOverM4aAAC(t *testing.T) {
	cands := []Format{
		audq(140, "mp4a.40.2", "m4a", 129000, QualityMedium),
		audq(251, "opus", "webm", 105000, QualityMedium),
	}
	idx, err := BestForTarget(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cands[idx]; got.Codec != "opus" || got.Extension != "webm" {
		t.Fatalf("chose %+v, want an opus/webm format", got)
	}
}

// audchan builds an audio format with an explicit channel count.
func audchan(itag int, codec string, avgBitrate, channels int) Format {
	f := aud(itag, codec, avgBitrate)
	f.Channels = channels
	return f
}

func TestWithChannels_StereoBeatsSurround(t *testing.T) {
	// Itag 258 is 5.1 surround at a much higher bitrate, but the stereo preference
	// must select the native stereo track.
	cands := []Format{
		audchan(258, "mp4a.40.2", 387000, 6),
		audchan(140, "mp4a.40.2", 128000, 2),
	}
	idx, err := BestAudio().WithChannels(LayoutStereo).Select(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 140 {
		t.Fatalf("stereo preference chose itag %d, want 140 (native stereo over 5.1)", cands[idx].Itag)
	}
}

func TestWithChannels_SurroundAndMono(t *testing.T) {
	cands := []Format{
		audchan(258, "mp4a.40.2", 387000, 6),
		audchan(140, "mp4a.40.2", 128000, 2),
		audchan(599, "mp4a.40.5", 32000, 1),
	}
	cases := map[ChannelLayout]int{
		LayoutSurround: 258,
		LayoutStereo:   140,
		LayoutMono:     599,
	}
	for layout, wantItag := range cases {
		idx, err := BestAudio().WithChannels(layout).Select(cands, MinimizeLoss(), Target{})
		if err != nil {
			t.Fatalf("%s: %v", layout, err)
		}
		if cands[idx].Itag != wantItag {
			t.Errorf("%s preference chose itag %d, want %d", layout, cands[idx].Itag, wantItag)
		}
	}
}

func TestWithChannels_AnyIsNeutral(t *testing.T) {
	// LayoutAny must reproduce the neutral bitrate-ranked result (258 surround).
	cands := []Format{
		audchan(258, "mp4a.40.2", 387000, 6),
		audchan(140, "mp4a.40.2", 128000, 2),
	}
	idx, err := BestAudio().WithChannels(LayoutAny).Select(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	neutral, _ := BestForTarget(cands, MinimizeLoss(), Target{})
	if idx != neutral || cands[idx].Itag != 258 {
		t.Fatalf("LayoutAny chose itag %d (idx %d), want the neutral winner 258 (idx %d)", cands[idx].Itag, idx, neutral)
	}
}

func TestWithChannels_NoNativeMatchFallsBack(t *testing.T) {
	// No mono track exists; the preference is inert and the best available wins.
	cands := []Format{
		audchan(258, "mp4a.40.2", 387000, 6),
		audchan(140, "mp4a.40.2", 128000, 2),
	}
	idx, err := BestAudio().WithChannels(LayoutMono).Select(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 258 {
		t.Fatalf("no-native-match chose itag %d, want best available 258", cands[idx].Itag)
	}
}

func TestWithChannels_UnknownCountInert(t *testing.T) {
	// Channels==0 (unknown) never matches, so a stereo preference cannot regress a
	// set with no channel metadata: bitrate still decides.
	cands := []Format{
		aud(251, "opus", 160000),
		aud(140, "mp4a.40.2", 128000),
	}
	idx, err := BestAudio().WithChannels(LayoutStereo).Select(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 251 {
		t.Fatalf("unknown-channels chose itag %d, want bitrate winner 251", cands[idx].Itag)
	}
}

func TestWithChannels_OriginalOutranksLayout(t *testing.T) {
	// Original-language audio takes precedence over a matching-layout dub.
	cands := []Format{
		{Itag: 258, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 387000, Channels: 6, IsOriginal: Yes},
		{Itag: 140, MIMEType: "audio/mp4", Codec: "mp4a.40.2", AverageBitrate: 128000, Channels: 2, IsOriginal: No},
	}
	idx, err := BestAudio().WithChannels(LayoutStereo).Select(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 258 {
		t.Fatalf("chose itag %d, want the original 258 even though 140 matches the stereo layout", cands[idx].Itag)
	}
}

func TestWithChannels_CodecFilterThenLayout(t *testing.T) {
	// --codec restricts to opus; the layout refines within that codec set.
	cands := []Format{
		audchan(251, "opus", 160000, 6),
		audchan(250, "opus", 70000, 2),
		audchan(140, "mp4a.40.2", 128000, 2),
	}
	idx, err := Codec("opus").WithChannels(LayoutStereo).Select(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 250 {
		t.Fatalf("codec+layout chose itag %d, want the stereo opus 250", cands[idx].Itag)
	}
}

func TestWithChannels_LayoutOutranksPreferCodec(t *testing.T) {
	// prefer:opus ranks below the layout, so the stereo aac track wins over a
	// surround opus track.
	cands := []Format{
		audchan(251, "opus", 160000, 6),
		audchan(140, "mp4a.40.2", 128000, 2),
	}
	idx, err := BestAudio().WithChannels(LayoutStereo).Select(cands, PreferCodec("opus"), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 140 {
		t.Fatalf("layout vs prefer:opus chose itag %d, want stereo aac 140 (layout outranks codec)", cands[idx].Itag)
	}
}

func TestWithChannels_ItagIgnoresLayout(t *testing.T) {
	// An itag selector names an exact encoding; the layout must not override it.
	cands := []Format{
		audchan(258, "mp4a.40.2", 387000, 6),
		audchan(140, "mp4a.40.2", 128000, 2),
	}
	idx, err := Itag(258).WithChannels(LayoutStereo).Select(cands, MinimizeLoss(), Target{})
	if err != nil {
		t.Fatal(err)
	}
	if cands[idx].Itag != 258 {
		t.Fatalf("itag selector chose itag %d, want the named 258 regardless of layout", cands[idx].Itag)
	}
}

func TestBetterThan_StrictWeakOrdering(t *testing.T) {
	set := []Format{
		{Itag: 1, Codec: "opus", AverageBitrate: 160000, AudioQuality: QualityHigh, IsOriginal: Yes, IsDRC: No, Channels: 2},
		{Itag: 2, Codec: "mp4a.40.2", AverageBitrate: 128000, AudioQuality: QualityHigh, IsOriginal: Yes, IsDRC: No, Channels: 2},
		{Itag: 3, Codec: "opus", AverageBitrate: 105000, AudioQuality: QualityMedium, IsOriginal: Yes, IsDRC: Yes, Channels: 6},
		{Itag: 4, Codec: "vorbis", AverageBitrate: 128000, AudioQuality: QualityMedium, IsOriginal: Unknown, IsDRC: No, Channels: 1},
		{Itag: 5, Codec: "mp4a.40.2", AverageBitrate: 200000, AudioQuality: QualityUnknown, IsOriginal: No, IsDRC: No, Channels: 6},
		{Itag: 6, Codec: "opus", AverageBitrate: 60000, AudioQuality: QualityUltraLow, IsOriginal: Yes, IsDRC: No, Channels: 0},
		{Itag: 7, Codec: "mp3", AverageBitrate: 128000, AudioQuality: QualityLow, IsOriginal: Yes, IsDRC: No, Channels: 2},
	}
	for _, prefCodec := range []string{"", "aac"} {
		for _, layout := range []ChannelLayout{LayoutAny, LayoutStereo, LayoutMono, LayoutSurround} {
			for _, useTier := range []bool{false, true} {
				name := fmt.Sprintf("prefCodec=%q/layout=%s/useTier=%v", prefCodec, layout, useTier)
				checkStrictWeakOrdering(t, name, set, betterThan(prefCodec, layout, useTier))
			}
		}
	}
}

// checkStrictWeakOrdering verifies the properties of a strict weak order.
func checkStrictWeakOrdering(t *testing.T, name string, set []Format, lt func(a, b Format) bool) {
	t.Helper()
	eq := func(a, b Format) bool { return !lt(a, b) && !lt(b, a) }
	for i := range set {
		if lt(set[i], set[i]) {
			t.Errorf("%s: not irreflexive at itag %d", name, set[i].Itag)
		}
	}
	for i := range set {
		for j := range set {
			if lt(set[i], set[j]) && lt(set[j], set[i]) {
				t.Errorf("%s: not asymmetric for itags %d,%d", name, set[i].Itag, set[j].Itag)
			}
		}
	}
	for i := range set {
		for j := range set {
			for k := range set {
				if lt(set[i], set[j]) && lt(set[j], set[k]) && !lt(set[i], set[k]) {
					t.Errorf("%s: < not transitive for itags %d,%d,%d", name, set[i].Itag, set[j].Itag, set[k].Itag)
				}
				if eq(set[i], set[j]) && eq(set[j], set[k]) && !eq(set[i], set[k]) {
					t.Errorf("%s: equivalence not transitive for itags %d,%d,%d", name, set[i].Itag, set[j].Itag, set[k].Itag)
				}
			}
		}
	}
}

func TestSelect_OrderIndependentNoTies(t *testing.T) {
	base := []Format{
		audq(251, "opus", "webm", 105000, QualityMedium),
		audq(140, "mp4a.40.2", "m4a", 129000, QualityMedium),
		audq(139, "mp4a.40.5", "m4a", 49000, QualityLow),
		audq(171, "vorbis", "webm", 128000, QualityLow),
	}
	const wantItag = 251
	permuteFormats(base, func(p []Format) {
		idx, err := BestForTarget(p, MinimizeLoss(), Target{})
		if err != nil {
			t.Fatal(err)
		}
		if p[idx].Itag != wantItag {
			t.Fatalf("permutation %v: winner itag=%d, want %d", itagsOf(p), p[idx].Itag, wantItag)
		}
	})
}

// permuteFormats invokes fn with every permutation of fs (each a fresh copy).
func permuteFormats(fs []Format, fn func([]Format)) {
	a := append([]Format(nil), fs...)
	var rec func(int)
	rec = func(i int) {
		if i == len(a) {
			fn(append([]Format(nil), a...))
			return
		}
		for j := i; j < len(a); j++ {
			a[i], a[j] = a[j], a[i]
			rec(i + 1)
			a[i], a[j] = a[j], a[i]
		}
	}
	rec(0)
}

func itagsOf(fs []Format) []int {
	out := make([]int, len(fs))
	for i := range fs {
		out[i] = fs[i].Itag
	}
	return out
}
