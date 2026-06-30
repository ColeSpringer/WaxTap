package waxtap

import (
	"testing"

	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/transcode"
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

// TestProbeMutationShiftsSelection shows why InfoResult must return the index it
// probed: applyProbe mutates the selected row in place, and re-selecting on the
// mutated slice can land on a different near-tie row. The probed numbers live on
// the originally selected row, so a re-selection would label an unprobed row.
func TestProbeMutationShiftsSelection(t *testing.T) {
	// Two near-tie stereo Opus rows differing only by manifest bitrate; itag 251
	// wins before the probe.
	formats := []Format{
		{Itag: 251, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 2, Bitrate: 200000, IsOriginal: Yes},
		{Itag: 250, Codec: "opus", Extension: "webm", MIMEType: "audio/webm", Channels: 2, Bitrate: 190000, IsOriginal: Yes},
	}
	sel := BestAudio().WithChannels(LayoutStereo)
	idx, err := selectIndex(sel, MinimizeLoss(), format.Target{}, formats)
	if err != nil {
		t.Fatalf("selectIndex: %v", err)
	}

	// A probe of the selected row corrects its bitrate below the runner-up's.
	applyProbe(&formats[idx], transcode.ProbeResult{
		Streams: []transcode.ProbeStream{{CodecType: "audio", SampleRate: 48000, Channels: 2, BitRate: 180000}},
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
	if formats[idx].Bitrate != 180000 {
		t.Errorf("probed row (idx %d) bitrate = %d, want 180000", idx, formats[idx].Bitrate)
	}
	if formats[reIdx].Bitrate == 180000 {
		t.Errorf("re-selected row (idx %d) should not carry the probed bitrate", reIdx)
	}
}
