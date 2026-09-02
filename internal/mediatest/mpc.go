package mediatest

import (
	_ "embed"
	"slices"
	"time"
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
