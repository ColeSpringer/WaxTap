package mediatest

import (
	_ "embed"
	"slices"
	"time"

	"github.com/colespringer/waxlabel"
)

// chapteredWMA is WaxFlow's container/asf chapter fixture, checked in here
// because nothing in WaxTap can write WMA (the engine decodes it only, and
// encoding it is an upstream non-goal): two seconds of a 440 Hz sine as 8 kHz
// mono WMA v2, written by ffmpeg's ASF muxer from an ffmetadata file, whose
// chapter list became the Marker Object ChapteredWMAChapters lists. It carries
// one text tag, ChapteredWMATags, beside the muxer's own encoder stamp. As
// ffmpeg wrote it, the Header Object declares six children and holds seven;
// the readers walk the header by object size and take the file cleanly
// (WaxFlow's strict mode included), so it stands for the WMA files that
// muxer leaves in the wild.
//
//go:embed testdata/chapters.wma
var chapteredWMA []byte

// ChapteredWMA returns a copy of the chaptered WMA fixture.
func ChapteredWMA() []byte { return slices.Clone(chapteredWMA) }

// The fixture's shape: 8 kHz mono, ChapteredWMASamples samples by the
// header's play duration. ASF states that duration in milliseconds and WMA
// carries no padding count, so the count is what a probe reports, not what a
// decode delivers: the engine emits whole frames of ChapteredWMAFrame samples
// (its frame rule for a rate at or under 16 kHz) and a decode runs to the
// frame boundary past the declared end.
const (
	ChapteredWMARate           = 8000
	ChapteredWMASamples  int64 = 16000
	ChapteredWMAFrame    int64 = 512
	ChapteredWMADuration       = time.Duration(ChapteredWMASamples) * time.Second / ChapteredWMARate
)

// ChapteredWMATags are the text tags the fixture carries, under the canonical
// keys both the engine's probe and WaxLabel report them by. The encoder stamp
// beside them is not listed: a carry excludes it by policy.
var ChapteredWMATags = map[string]string{"TITLE": "Chaptered"}

// ChapteredWMAChapters returns the fixture's markers as both the engine and
// WaxLabel read them: on the playback timeline (the muxer's pre-roll taken
// off), in start order, the start-only form (End zero), one title past ASCII.
func ChapteredWMAChapters() []waxlabel.Chapter {
	return []waxlabel.Chapter{
		{Start: 0, Title: "Intro"},
		{Start: 500 * time.Millisecond, Title: "Mïddle"},
		{Start: 1250 * time.Millisecond, Title: "Coda"},
	}
}
