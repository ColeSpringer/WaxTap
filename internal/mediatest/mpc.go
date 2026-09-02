package mediatest

import (
	_ "embed"
	"slices"
	"time"

	"github.com/colespringer/waxlabel"
)

// taggedMPC is WaxFlow's own container/mpc fixture, checked in here because
// WaxTap has no Musepack encoder to synthesize one with (encoding it is an
// upstream non-goal): a Musepack SV7 stream the reference encoder wrote from a
// synthetic seed, carrying the APEv2 tag TaggedMPCTags lists.
//
//go:embed testdata/tagged.mpc
var taggedMPC []byte

// TaggedMPC returns a copy of the tagged Musepack fixture.
func TaggedMPC() []byte { return slices.Clone(taggedMPC) }

// The fixture's shape: 44.1 kHz stereo, TaggedMPCSamples samples after the
// format's gapless trim, which is what a probe reports and what a lossless
// encode of it delivers.
const (
	TaggedMPCRate           = 44100
	TaggedMPCSamples  int64 = 8000
	TaggedMPCDuration       = time.Duration(TaggedMPCSamples) * time.Second / TaggedMPCRate
)

// TaggedMPCTags are the APEv2 items the fixture carries, under the canonical
// keys both the engine's probe and WaxLabel report them by.
var TaggedMPCTags = map[string]string{
	"ARTIST": "Wax Test", "ALBUM": "Fixtures", "TITLE": "Tagged", "RECORDINGDATE": "2026", "TRACKNUMBER": "3",
}

// chapteredMPC is WaxFlow's container/mpc chapter fixture: a Musepack SV8
// stream the reference encoder wrote from a synthetic seed, with the chapters
// ChapteredMPCChapters lists written into it by mpcchap, the reference chapter
// editor and the only writer of SV8 chapter packets. The file carries no
// APEv2 tag of its own; the items it holds sit inside the chapter packets
// (Middle's names an artist and a track number beside its title), which a
// carry must treat as the chapters' metadata, never the file's. WaxLabel's
// testdata carries its own render of the same seed and chapter list under
// this name, in different bytes; this copy is WaxFlow's, the reader that
// decodes it here.
//
//go:embed testdata/chapters.mpc
var chapteredMPC []byte

// ChapteredMPC returns a copy of the chaptered Musepack fixture: 44.1 kHz
// stereo, 20000 samples after the gapless trim.
func ChapteredMPC() []byte { return slices.Clone(chapteredMPC) }

// ChapteredMPCRate is the chaptered fixture's sample rate, the timeline its
// chapter starts are placed on.
const ChapteredMPCRate = 44100

// ChapteredMPCChapters returns the fixture's chapters as both the engine and
// WaxLabel read them: the start-only form (End zero, "until the next
// chapter"), in start order although the editor wrote them out of it, and the
// last one untitled because it carries no tag bytes at all. Starts are the
// packets' sample offsets (0, 8000, 16000, 19000) on the 44.1 kHz timeline.
func ChapteredMPCChapters() []waxlabel.Chapter {
	return []waxlabel.Chapter{
		{Start: 0, Title: "Intro"},
		{Start: time.Duration(8000) * time.Second / ChapteredMPCRate, Title: "Middle"},
		{Start: time.Duration(16000) * time.Second / ChapteredMPCRate, Title: "Coda"},
		{Start: time.Duration(19000) * time.Second / ChapteredMPCRate, Title: ""},
	}
}
