package mediatest

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/mp4"
	"github.com/colespringer/waxflow/format"
)

// FragmentAAC reshapes a progressive AAC MP4 into the fragmented shape
// YouTube's itag 140 has: an init segment whose moov carries an empty sample
// table and no edit list, a segment index listing every fragment, then one
// styp+moof+mdat per fragment. The length is stated by the sidx alone, which
// is the tier a header-sized open reads; nothing in the head is exact until
// the fragments are walked. The sidx counts the fragments' raw frames, so the
// length it states carries the encoder's priming and padding.
//
// It returns an error rather than taking a testing.TB, like every other helper
// here: a fixture builder is a library function, and importing testing into one
// puts the flag set into whatever links it.
func FragmentAAC(progressive []byte) ([]byte, error) {
	demux, info, err := format.OpenDemuxer(container.BytesSource(progressive), "m4a", nil)
	if err != nil {
		return nil, fmt.Errorf("FragmentAAC: open the progressive source: %w", err)
	}
	track := info.Default()
	// No edit list: an init that states a delay or a length gets one, and the
	// open would then take the length from it rather than from the sidx.
	track.Samples, track.Delay, track.Padding = -1, 0, 0
	init, err := mp4.InitSegment(track)
	if err != nil {
		return nil, fmt.Errorf("FragmentAAC: init segment: %w", err)
	}
	// 43 AAC frames of 1024 samples, about one second: a segment boundary has
	// to fall on a frame, since a packet cannot straddle one.
	seg, err := mp4.NewSegmenter(track, &mp4.SegmenterOptions{SegmentSamples: 43 * 1024})
	if err != nil {
		return nil, fmt.Errorf("FragmentAAC: segmenter: %w", err)
	}
	var segments []mp4.Segment
	emit := func(s mp4.Segment) error {
		segments = append(segments, s)
		return nil
	}
	var pkt container.Packet
	for {
		err := demux.ReadPacket(&pkt)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("FragmentAAC: read packet: %w", err)
		}
		if err := seg.WritePacket(pkt.Packet, emit); err != nil {
			return nil, fmt.Errorf("FragmentAAC: write packet: %w", err)
		}
	}
	if err := seg.End(emit); err != nil {
		return nil, fmt.Errorf("FragmentAAC: end: %w", err)
	}
	// reference_count is 16 bits, so an index past that many fragments would
	// wrap into the reserved half and read as a shorter one.
	if len(segments) > 0xffff {
		return nil, fmt.Errorf("FragmentAAC: %d fragments; a sidx indexes at most %d", len(segments), 0xffff)
	}
	out := append([]byte(nil), init...)
	out = append(out, sidxBox(track.Fmt.Rate, segments)...)
	for _, s := range segments {
		out = append(out, s.Data...)
	}
	return out, nil
}

// sidxBox builds a version-0 segment index over segments, on the track's own
// timescale and starting right after the box, so its coverage runs to the end
// of the file.
func sidxBox(timescale int, segments []mp4.Segment) []byte {
	payload := make([]byte, 0, 24+12*len(segments))
	put := func(v uint32) { payload = binary.BigEndian.AppendUint32(payload, v) }
	put(0)                     // version 0, flags 0
	put(1)                     // reference_ID: the one track
	put(uint32(timescale))     // timescale
	put(0)                     // earliest_presentation_time
	put(0)                     // first_offset: the fragments follow the box
	put(uint32(len(segments))) // reserved (16) | reference_count (16)
	for _, s := range segments {
		put(uint32(len(s.Data))) // reference_type 0 | referenced_size
		put(uint32(s.Samples))   // subsegment_duration
		put(0x80000000)          // starts_with_SAP, SAP_type 0, SAP_delta_time 0
	}
	box := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
	box = append(box, "sidx"...)
	return append(box, payload...)
}
