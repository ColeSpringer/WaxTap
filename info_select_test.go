package waxtap

import (
	"testing"
	"time"

	"github.com/colespringer/waxtap/v3/format"
	"github.com/colespringer/waxtap/v3/internal/media"
)

// TestSelectIndexHonorsChannelPreference is the index-mismatch core of the
// info --probe fix: under MinimizeLoss with no channel preference a higher-bitrate
// surround track outranks stereo, so InfoResult must select with the same channel
// preference the CLI displays rather than LayoutAny.
func TestSelectIndexHonorsChannelPreference(t *testing.T) {
	// Identical except for channels and bitrate: with no layout preference the
	// 6ch/256k track wins on bitrate; with a stereo preference the 2ch track wins
	// because the channel match ranks above bitrate.
	formats := []Format{
		{Itag: 251, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 2, AverageBitrate: 160000, IsOriginal: Yes},
		{Itag: 338, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 6, AverageBitrate: 256000, IsOriginal: Yes},
	}

	anyIdx, err := selectIndex(BestAudio().WithChannels(LayoutAny), MinimizeLoss(), format.Target{}, formats)
	if err != nil {
		t.Fatalf("selectIndex any: %v", err)
	}
	stereoIdx, err := selectIndex(BestAudio().WithChannels(LayoutStereo), MinimizeLoss(), format.Target{}, formats)
	if err != nil {
		t.Fatalf("selectIndex stereo: %v", err)
	}

	if formats[anyIdx].Channels != 6 {
		t.Errorf("LayoutAny selected %dch (itag %d), want the 6ch surround track", formats[anyIdx].Channels, formats[anyIdx].Itag)
	}
	if formats[stereoIdx].Channels != 2 {
		t.Errorf("LayoutStereo selected %dch (itag %d), want the 2ch stereo track", formats[stereoIdx].Channels, formats[stereoIdx].Itag)
	}
	if anyIdx == stereoIdx {
		t.Fatal("channel preference must change the selected index; otherwise the probe/display mismatch cannot occur")
	}
}

// TestFacadeDefaultsToStereo pins the F5 contract by driving the real seam code, so
// dropping the default wiring from a seam fails this test rather than shipping green:
//   - Download: the actual selectSourceIndex on a bare Request must pick stereo.
//   - Info: newReadOptions must seed the facade default, overridable by WithChannels.
//   - Resolve: the zero AudioSelector, transformed as Resolve transforms it, picks
//     stereo (selector-level: Resolve needs a live extraction to run end to end).
//
// Opting into surround or any-fidelity still reaches the surround track. The
// format-package ranking is unchanged (see TestSelectIndexHonorsChannelPreference).
func TestFacadeDefaultsToStereo(t *testing.T) {
	c := newOfflineClient(t)
	// The 6ch/256k track outranks 2ch/160k on bitrate under LayoutAny, so a missing
	// default would hand back surround.
	formats := []Format{
		{Itag: 251, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 2, AverageBitrate: 160000, IsOriginal: Yes},
		{Itag: 338, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 6, AverageBitrate: 256000, IsOriginal: Yes},
	}

	// Download seam: the actual selection path a Request takes inside Download/Stream.
	download := func(t *testing.T, req Request) Format {
		t.Helper()
		idx, err := c.selectSourceIndex(req, format.Target{}, formats, 0)
		if err != nil {
			t.Fatalf("selectSourceIndex: %v", err)
		}
		return formats[idx]
	}
	if f := download(t, Request{}); f.Itag != 251 || f.Channels != 2 {
		t.Errorf("bare Request selected itag %d (%dch), want the stereo itag 251", f.Itag, f.Channels)
	}
	if f := download(t, Request{Audio: BestAudio().WithChannels(LayoutAny)}); f.Channels != 6 {
		t.Errorf("Request WithChannels(LayoutAny) selected %dch, want the 6ch surround track", f.Channels)
	}
	if f := download(t, Request{Audio: BestAudio().WithChannels(LayoutSurround)}); f.Channels != 6 {
		t.Errorf("Request WithChannels(LayoutSurround) selected %dch, want 6ch surround", f.Channels)
	}

	// Info seam: newReadOptions seeds the facade default, and WithChannels overrides it.
	if got := newReadOptions(nil).layout; got != defaultFacadeLayout {
		t.Errorf("newReadOptions default layout = %v, want the facade default %v", got, defaultFacadeLayout)
	}
	if got := newReadOptions([]ReadOption{WithChannels(LayoutAny)}).layout; got != LayoutAny {
		t.Errorf("WithChannels(LayoutAny) read option layout = %v, want LayoutAny (override honored)", got)
	}

	// Cross-seam agreement: the selector Info builds from its default, and the one
	// Resolve wraps a zero selector into, both land on the same stereo itag as Download.
	agree := func(t *testing.T, name string, sel AudioSelector) {
		t.Helper()
		idx, err := selectIndex(sel, MinimizeLoss(), format.Target{}, formats)
		if err != nil {
			t.Fatalf("%s selectIndex: %v", name, err)
		}
		if formats[idx].Itag != 251 {
			t.Errorf("%s selected itag %d, want the stereo itag 251", name, formats[idx].Itag)
		}
	}
	agree(t, "info", BestAudio().WithChannels(newReadOptions(nil).layout))
	agree(t, "resolve", AudioSelector{}.WithDefaultChannels(defaultFacadeLayout))
}

// TestProbeMutationShiftsSelection shows why InfoResult must return the index it
// probed: applyProbe mutates the selected row in place, and re-selecting on the
// mutated slice can land on a different near-tie row. The probed numbers live on
// the originally selected row, so a re-selection would label an unprobed row.
func TestProbeMutationShiftsSelection(t *testing.T) {
	// Two rows tied on every ranked field, both claiming stereo with unknown
	// bitrate; itag 251 wins the tie by coming first.
	formats := []Format{
		{Itag: 251, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 2, IsOriginal: Yes},
		{Itag: 250, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 2, IsOriginal: Yes},
	}
	sel := BestAudio().WithChannels(LayoutStereo)
	idx, err := selectIndex(sel, MinimizeLoss(), format.Target{}, formats)
	if err != nil {
		t.Fatalf("selectIndex: %v", err)
	}
	if formats[idx].Itag != 251 {
		t.Fatalf("selectIndex chose itag %d, want 251 (first on the tie)", formats[idx].Itag)
	}

	// A probe of the selected row corrects the manifest's channel claim to the
	// true 6-channel layout, which outranks bitrate and so drops the row's
	// stereo-layout match; it also fills the size-derived content length and
	// bitrate estimate (the audio stream itself reports neither).
	applyProbe(&formats[idx], media.ProbeResult{
		Format:  media.ProbeFormat{Duration: 10 * time.Second, Size: 225000},
		Streams: []media.ProbeStream{{CodecType: "audio", SampleRate: 48000, Channels: 6}},
	})

	reIdx, err := selectIndex(sel, MinimizeLoss(), format.Target{}, formats)
	if err != nil {
		t.Fatalf("selectIndex after probe: %v", err)
	}
	if reIdx == idx {
		t.Fatal("expected the probe correction to shift the near-tie selection")
	}
	// The probed numbers are on idx, not reIdx: re-selecting would display an
	// unprobed row labeled (probed). InfoResult.BestIndex returns idx to prevent that.
	if formats[idx].Channels != 6 {
		t.Errorf("probed row (idx %d) Channels = %d, want the corrected 6", idx, formats[idx].Channels)
	}
	if formats[idx].ContentLength != 225000 {
		t.Errorf("probed row (idx %d) ContentLength = %d, want the probed size 225000", idx, formats[idx].ContentLength)
	}
	if want := int(float64(225000) * 8 / 10); formats[idx].Bitrate != want {
		t.Errorf("probed row (idx %d) Bitrate = %d, want the size/duration estimate %d", idx, formats[idx].Bitrate, want)
	}
	if formats[reIdx].Channels != 2 {
		t.Errorf("re-selected row (idx %d) Channels = %d, want the still-stereo 2", reIdx, formats[reIdx].Channels)
	}
}

// WithSelector and WithChannels can both speak for the channel layout, so the
// precedence has to be pinned rather than left to whichever is applied last: a
// selector carries the whole intent (which encoding, in which layout), while
// WithChannels is a preference for one field of it.
//
// The layout is unexported, so this asserts through selection itself, which is
// what the precedence actually decides.
func TestReadOptionSelectorBeatsChannels(t *testing.T) {
	// One surround row and one stereo row, otherwise identical, so the chosen
	// index names the layout that won.
	formats := []Format{
		{Itag: 1, Codec: "opus", Channels: 6, Bitrate: 128000},
		{Itag: 2, Codec: "opus", Channels: 2, Bitrate: 128000},
	}
	pick := func(t *testing.T, opts ...ReadOption) int {
		t.Helper()
		ro := newReadOptions(opts)
		idx, err := ro.sel.WithDefaultChannels(ro.layout).Select(formats, ro.policy, Target{})
		if err != nil {
			t.Fatalf("Select: %v", err)
		}
		return formats[idx].Itag
	}

	t.Run("the selector's layout wins", func(t *testing.T) {
		if got := pick(t, WithChannels(LayoutStereo), WithSelector(BestAudio().WithChannels(LayoutSurround))); got != 1 {
			t.Errorf("chose itag %d, want the selector's surround row", got)
		}
	})

	// Order must not decide it: the same pair the other way round is the same
	// request.
	t.Run("order does not matter", func(t *testing.T) {
		if got := pick(t, WithSelector(BestAudio().WithChannels(LayoutSurround)), WithChannels(LayoutStereo)); got != 1 {
			t.Errorf("chose itag %d, want the selector's surround row", got)
		}
	})

	// A selector that expressed no layout is exactly what WithChannels is for.
	t.Run("channels fill in a selector that named none", func(t *testing.T) {
		if got := pick(t, WithChannels(LayoutSurround), WithSelector(BestAudio())); got != 1 {
			t.Errorf("chose itag %d, want surround to fill in", got)
		}
	})

	t.Run("source policy is carried", func(t *testing.T) {
		ro := newReadOptions([]ReadOption{WithSourcePolicy(PreferCodec("opus"))})
		if got := ro.policy.Preferred(); got != "opus" {
			t.Errorf("preferred = %q, want opus", got)
		}
	})
}
