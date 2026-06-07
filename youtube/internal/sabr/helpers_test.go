package sabr

import (
	"bytes"
	"encoding/binary"
	"io"
	"net/http"
	"testing"
)

// Test-only encoders for UMP frames and protobuf response messages. Production
// code only decodes these responses, so the encoders live with the tests that
// build fixtures.

// umpVarint encodes v as a UMP variable-length integer, the inverse of
// umpReader.readVarint. It does not use the production decoder, keeping the
// round-trip test independent.
func umpVarint(v uint64) []byte {
	switch {
	case v < 1<<7:
		return []byte{byte(v)}
	case v < 1<<14:
		return []byte{0x80 | byte(v>>8), byte(v)}
	case v < 1<<21:
		return []byte{0xC0 | byte(v>>16), byte(v), byte(v >> 8)}
	case v < 1<<28:
		return []byte{0xE0 | byte(v>>24), byte(v), byte(v >> 8), byte(v >> 16)}
	default:
		b := []byte{0xF0, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(b[1:], uint32(v))
		return b
	}
}

// umpFrame wraps a payload as one UMP part: varint(type) varint(size) payload.
func umpFrame(partType int, payload []byte) []byte {
	var b []byte
	b = append(b, umpVarint(uint64(partType))...)
	b = append(b, umpVarint(uint64(len(payload)))...)
	return append(b, payload...)
}

// mediaFrame builds a MEDIA part: a leading header_id varint then raw bytes.
func mediaFrame(headerID uint32, media []byte) []byte {
	payload := append(umpVarint(uint64(headerID)), media...)
	return umpFrame(partMedia, payload)
}

// concat joins UMP part frames into one response body.
func concat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

func marshalMediaHeader(h MediaHeader) []byte {
	var b []byte
	b = appendVarint(b, fMediaHdrHeaderID, uint64(h.HeaderID))
	if h.Itag != 0 {
		b = appendVarint(b, fMediaHdrItag, uint64(h.Itag))
	}
	if h.LastModified != 0 {
		b = appendVarint(b, fMediaHdrLastModified, h.LastModified)
	}
	if h.XTags != "" {
		b = appendString(b, fMediaHdrXTags, h.XTags)
	}
	if h.IsInitSeg {
		b = appendVarint(b, fMediaHdrIsInitSeg, 1)
	}
	if h.SequenceNumber != 0 {
		b = appendVarint(b, fMediaHdrSequenceNum, h.SequenceNumber)
	}
	if h.StartMs != 0 {
		b = appendVarint(b, fMediaHdrStartMs, uint64(h.StartMs))
	}
	if h.DurationMs != 0 {
		b = appendVarint(b, fMediaHdrDurationMs, uint64(h.DurationMs))
	}
	if h.FormatId != (FormatId{}) {
		b = appendBytes(b, fMediaHdrFormatID, h.FormatId.marshal())
	}
	if h.ContentLength != 0 {
		b = appendVarint(b, fMediaHdrContentLength, uint64(h.ContentLength))
	}
	return b
}

func marshalByteRange(r ByteRange) []byte {
	var b []byte
	b = appendVarint(b, fRangeStart, uint64(r.Start))
	b = appendVarint(b, fRangeEnd, uint64(r.End))
	return b
}

func marshalFormatInit(m FormatInitializationMetadata) []byte {
	var b []byte
	if m.FormatId != (FormatId{}) {
		b = appendBytes(b, fFmtInitFormatID, m.FormatId.marshal())
	}
	if m.EndSegmentNumber != 0 {
		b = appendVarint(b, fFmtInitEndSegment, uint64(m.EndSegmentNumber))
	}
	if m.MimeType != "" {
		b = appendString(b, fFmtInitMimeType, m.MimeType)
	}
	if m.InitRange != (ByteRange{}) {
		b = appendBytes(b, fFmtInitInitRange, marshalByteRange(m.InitRange))
	}
	if m.IndexRange != (ByteRange{}) {
		b = appendBytes(b, fFmtInitIndexRange, marshalByteRange(m.IndexRange))
	}
	if m.DurationUnits != 0 {
		b = appendVarint(b, fFmtInitDurationUnits, uint64(m.DurationUnits))
	}
	if m.DurationTimescale != 0 {
		b = appendVarint(b, fFmtInitDurationScale, uint64(m.DurationTimescale))
	}
	return b
}

func marshalNextRequestPolicy(p NextRequestPolicy) []byte {
	var b []byte
	if p.TargetAudioReadaheadMs != 0 {
		b = appendVarint(b, fNextPolicyReadaheadMs, uint64(p.TargetAudioReadaheadMs))
	}
	if p.BackoffTimeMs != 0 {
		b = appendVarint(b, fNextPolicyBackoffMs, uint64(p.BackoffTimeMs))
	}
	if len(p.PlaybackCookie) > 0 {
		b = appendBytes(b, fNextPolicyCookie, p.PlaybackCookie)
	}
	return b
}

func marshalSabrRedirect(s SabrRedirect) []byte {
	return appendString(nil, fSabrRedirectURL, s.URL)
}

func marshalSabrError(s SabrError) []byte {
	var b []byte
	b = appendString(b, fSabrErrorType, s.Type)
	b = appendVarint(b, fSabrErrorCode, uint64(s.Code))
	return b
}

func marshalStreamProtectionStatus(s StreamProtectionStatus) []byte {
	var b []byte
	b = appendVarint(b, fProtStatusStatus, uint64(s.Status))
	if s.MaxRetries != 0 {
		b = appendVarint(b, fProtStatusMaxRetries, uint64(s.MaxRetries))
	}
	return b
}

// doerFunc adapts a function to the HTTPDoer interface.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// scriptedDoer replays one prepared response body per call and records the
// request URLs and bodies for assertions.
type scriptedDoer struct {
	t         *testing.T
	responses [][]byte
	statuses  []int // optional; 0 means 200
	calls     int
	urls      []string
	bodies    [][]byte
}

func (d *scriptedDoer) Do(req *http.Request) (*http.Response, error) {
	i := d.calls
	d.calls++
	d.urls = append(d.urls, req.URL.String())
	body, _ := io.ReadAll(req.Body)
	d.bodies = append(d.bodies, body)
	if i >= len(d.responses) {
		d.t.Fatalf("unexpected SABR request #%d to %s", i, req.URL)
	}
	status := http.StatusOK
	if i < len(d.statuses) && d.statuses[i] != 0 {
		status = d.statuses[i]
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(bytes.NewReader(d.responses[i])),
		Header:     make(http.Header),
	}, nil
}
