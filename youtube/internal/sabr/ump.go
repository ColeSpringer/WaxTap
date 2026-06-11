package sabr

import (
	"encoding/binary"
	"errors"
)

// UMP responses are sequences of (part type, payload size, payload) tuples. Type
// and size use UMP's variable-length integer encoding rather than protobuf
// varints. Unknown part IDs can be skipped from their encoded size.

// UMP part ids (verified against the pinned upstream revision; see proto.go's
// upstreamCommit and protos/video_streaming/ump_part_id.proto).
const (
	partMediaHeader        = 20
	partMedia              = 21
	partMediaEnd           = 22
	partNextRequestPolicy  = 35
	partFormatInitMetadata = 42
	partSabrRedirect       = 43
	partSabrError          = 44
	partReloadPlayerResp   = 46
	partSabrContextUpdate  = 57
	partStreamProtection   = 58
	partSabrContextSendPol = 59
)

// errUMPTruncated marks a UMP stream that ended mid-varint or mid-part.
var errUMPTruncated = errors.New("sabr: truncated UMP stream")

// umpReader reads UMP framing from a fully-buffered response body.
type umpReader struct {
	b   []byte
	pos int
}

func newUMPReader(b []byte) *umpReader { return &umpReader{b: b} }

// umpPart is one decoded part. Payload aliases the reader's buffer.
type umpPart struct {
	Type    int
	Payload []byte
}

// next returns the next part, or ok=false at a clean end of stream. A partial
// part at the end returns an error.
func (u *umpReader) next() (part umpPart, ok bool, err error) {
	if u.pos >= len(u.b) {
		return umpPart{}, false, nil
	}
	partType, err := u.readVarint()
	if err != nil {
		return umpPart{}, false, err
	}
	partSize, err := u.readVarint()
	if err != nil {
		return umpPart{}, false, err
	}
	end := u.pos + int(partSize)
	if end < u.pos || end > len(u.b) {
		return umpPart{}, false, errUMPTruncated
	}
	payload := u.b[u.pos:end]
	u.pos = end
	return umpPart{Type: int(partType), Payload: payload}, true, nil
}

// readVarint reads a UMP variable-length integer. The first byte's leading 1
// bits set the total length (1..5): 0xxxxxxx->1, 10xxxxxx->2, 110xxxxx->3,
// 1110xxxx->4, 11110xxx->5. For the 1..4 byte forms the prefix's trailing
// (8-size) low bits hold the value's low bits and each following byte stacks
// above them; the 5-byte form ignores the prefix's low bits and reads the next
// 4 bytes as a little-endian uint32. Matches LuanRT/googlevideo UmpReader.ts at
// the pinned upstreamCommit.
func (u *umpReader) readVarint() (uint64, error) {
	if u.pos >= len(u.b) {
		return 0, errUMPTruncated
	}
	prefix := u.b[u.pos]
	size := umpVarintSize(prefix)
	if u.pos+size > len(u.b) {
		return 0, errUMPTruncated
	}
	u.pos++
	if size == 5 {
		v := binary.LittleEndian.Uint32(u.b[u.pos : u.pos+4])
		u.pos += 4
		return uint64(v), nil
	}
	// Prefix's low (8-size) bits are the value's low bits; later bytes stack above.
	mask := byte(1)<<(8-uint(size)) - 1
	value := uint64(prefix & mask)
	shift := uint(8 - size)
	for i := 0; i < size-1; i++ {
		value |= uint64(u.b[u.pos]) << shift
		shift += 8
		u.pos++
	}
	return value, nil
}

// umpVarintSize returns the total byte length encoded by a UMP varint's first
// byte: 1 plus the count of leading 1 bits, clamped to 5.
func umpVarintSize(prefix byte) int {
	size := 1
	for i := 7; i >= 1; i-- {
		if prefix&(1<<uint(i)) == 0 {
			break
		}
		size++
	}
	if size > 5 {
		size = 5
	}
	return size
}

// leadingVarint reads a UMP varint from the front of payload and returns it with
// the remaining bytes. MEDIA parts begin with a header_id followed by raw media
// bytes; MEDIA_END parts carry only the header_id.
func leadingVarint(payload []byte) (value uint64, rest []byte, err error) {
	r := umpReader{b: payload}
	value, err = r.readVarint()
	if err != nil {
		return 0, nil, err
	}
	return value, payload[r.pos:], nil
}
