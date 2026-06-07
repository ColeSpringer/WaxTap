package sabr

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// These tests use literal field numbers so they fail when the production
// constants are wrong.

// pfield is one scanned protobuf field value.
type pfield struct {
	typ protowire.Type
	v   uint64 // set for varint fields
	b   []byte // set for length-delimited fields
}

// protoScan walks every field in buf into a map keyed by field number.
func protoScan(t *testing.T, buf []byte) map[protowire.Number][]pfield {
	t.Helper()
	out := map[protowire.Number][]pfield{}
	for len(buf) > 0 {
		num, typ, n := protowire.ConsumeTag(buf)
		if n < 0 {
			t.Fatalf("ConsumeTag: %v", protowire.ParseError(n))
		}
		buf = buf[n:]
		f := pfield{typ: typ}
		switch typ {
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(buf)
			if m < 0 {
				t.Fatalf("ConsumeVarint: %v", protowire.ParseError(m))
			}
			f.v, buf = v, buf[m:]
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(buf)
			if m < 0 {
				t.Fatalf("ConsumeBytes: %v", protowire.ParseError(m))
			}
			f.b, buf = append([]byte(nil), v...), buf[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, buf)
			if m < 0 {
				t.Fatalf("ConsumeFieldValue: %v", protowire.ParseError(m))
			}
			buf = buf[m:]
		}
		out[num] = append(out[num], f)
	}
	return out
}

// one returns the single value for field num, failing if absent or repeated.
func one(t *testing.T, fields map[protowire.Number][]pfield, num protowire.Number) pfield {
	t.Helper()
	vs := fields[num]
	if len(vs) != 1 {
		t.Fatalf("field %d: got %d values, want 1", num, len(vs))
	}
	return vs[0]
}

// Literal-number builders for crafting response payloads by hand.
func pv(num protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, num, protowire.VarintType), v)
}
func ps(num protowire.Number, s string) []byte {
	return protowire.AppendString(protowire.AppendTag(nil, num, protowire.BytesType), s)
}
func pb(num protowire.Number, raw []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, num, protowire.BytesType), raw)
}

func TestVideoPlaybackAbrRequestRoundTrip(t *testing.T) {
	req := videoPlaybackAbrRequest{
		ClientAbrState:    clientAbrState{PlayerTimeMs: 1234, EnabledTrackTypes: enabledTrackTypesAudioOnly},
		SelectedFormatIds: []FormatId{{Itag: 251, LastModified: 1700000000000001, XTags: "acont=original"}},
		BufferedRanges:    []BufferedRange{{FormatId: FormatId{Itag: 251}, DurationMs: 5000, StartSegmentIndex: 1, EndSegmentIndex: 3}},
		PlayerTimeMs:      1234,
		UstreamerConfig:   []byte("ustreamer-bytes"),
		StreamerContext: streamerContext{
			ClientInfo:     ClientInfo{ClientName: 1, ClientVersion: "2.x", OSName: "Windows", OSVersion: "10.0", AcceptLanguage: "en-US"},
			POToken:        []byte("po-token-bytes"),
			PlaybackCookie: []byte("cookie"),
		},
	}
	top := protoScan(t, req.marshal())

	// Top-level VideoPlaybackAbrRequest field numbers.
	if got := one(t, top, 4).v; got != 1234 {
		t.Errorf("player_time_ms(4) = %d, want 1234", got)
	}
	if got := one(t, top, 5).b; string(got) != "ustreamer-bytes" {
		t.Errorf("video_playback_ustreamer_config(5) = %q", got)
	}
	if len(top[2]) != 1 {
		t.Fatalf("selected_format_ids(2): got %d, want 1", len(top[2]))
	}
	if len(top[3]) != 1 {
		t.Fatalf("buffered_ranges(3): got %d, want 1", len(top[3]))
	}

	// client_abr_state (1): player_time_ms=28, enabled_track_types=40.
	cas := protoScan(t, one(t, top, 1).b)
	if got := one(t, cas, 28).v; got != 1234 {
		t.Errorf("client_abr_state.player_time_ms(28) = %d, want 1234", got)
	}
	if got := one(t, cas, 40).v; got != 1 {
		t.Errorf("client_abr_state.enabled_track_types(40) = %d, want 1 (audio only)", got)
	}

	// selected_format_ids[0] (misc.FormatId): itag=1, last_modified=2, xtags=3.
	fid := protoScan(t, top[2][0].b)
	if got := one(t, fid, 1).v; got != 251 {
		t.Errorf("FormatId.itag(1) = %d, want 251", got)
	}
	if got := one(t, fid, 2).v; got != 1700000000000001 {
		t.Errorf("FormatId.last_modified(2) = %d", got)
	}
	if got := one(t, fid, 3).b; string(got) != "acont=original" {
		t.Errorf("FormatId.xtags(3) = %q", got)
	}

	// streamer_context (19): client_info=1, po_token=2, playback_cookie=3.
	sc := protoScan(t, one(t, top, 19).b)
	if got := one(t, sc, 2).b; string(got) != "po-token-bytes" {
		t.Errorf("streamer_context.po_token(2) = %q", got)
	}
	if got := one(t, sc, 3).b; string(got) != "cookie" {
		t.Errorf("streamer_context.playback_cookie(3) = %q", got)
	}

	// client_info (1): device_make=12, device_model=13, client_name=16,
	// client_version=17, os_name=18, os_version=19, accept_language=21.
	ci := protoScan(t, one(t, sc, 1).b)
	if got := one(t, ci, 16).v; got != 1 {
		t.Errorf("client_info.client_name(16) = %d, want 1", got)
	}
	if got := one(t, ci, 17).b; string(got) != "2.x" {
		t.Errorf("client_info.client_version(17) = %q", got)
	}
	if got := one(t, ci, 18).b; string(got) != "Windows" {
		t.Errorf("client_info.os_name(18) = %q", got)
	}
	if got := one(t, ci, 19).b; string(got) != "10.0" {
		t.Errorf("client_info.os_version(19) = %q", got)
	}
	if got := one(t, ci, 21).b; string(got) != "en-US" {
		t.Errorf("client_info.accept_language(21) = %q", got)
	}
}

func TestUnmarshalMediaHeader(t *testing.T) {
	// header_id=1, itag=3, lmt=4, xtags=5, is_init_seg=8, sequence_number=9,
	// start_ms=11, duration_ms=12, format_id=13, content_length=14.
	body := bytes.Join([][]byte{
		pv(1, 7),
		pv(3, 140),
		pv(4, 1700000000000002),
		ps(5, "x=1"),
		pv(8, 1),
		pv(9, 5),
		pv(11, 1000),
		pv(12, 2000),
		pb(13, FormatId{Itag: 140}.marshal()),
		pv(14, 3400000),
	}, nil)

	h, err := unmarshalMediaHeader(body)
	if err != nil {
		t.Fatal(err)
	}
	if h.HeaderID != 7 || h.Itag != 140 || h.LastModified != 1700000000000002 || h.XTags != "x=1" {
		t.Errorf("identity = %+v", h)
	}
	if !h.IsInitSeg || h.SequenceNumber != 5 || h.StartMs != 1000 || h.DurationMs != 2000 || h.ContentLength != 3400000 {
		t.Errorf("segment fields = %+v", h)
	}
	if h.FormatId.Itag != 140 {
		t.Errorf("nested format_id.itag = %d, want 140", h.FormatId.Itag)
	}
}

func TestUnmarshalFormatInitMetadata(t *testing.T) {
	// format_id=2, end_segment_number=4, mime_type=5, init_range=6, index_range=7,
	// duration_units=9, duration_timescale=10. misc.Range: start=3, end=4.
	initRange := bytes.Join([][]byte{pv(3, 0), pv(4, 742)}, nil)
	indexRange := bytes.Join([][]byte{pv(3, 743), pv(4, 1200)}, nil)
	body := bytes.Join([][]byte{
		pb(2, FormatId{Itag: 251}.marshal()),
		pv(4, 130),
		ps(5, `audio/webm; codecs="opus"`),
		pb(6, initRange),
		pb(7, indexRange),
		pv(9, 1000),
		pv(10, 48000),
	}, nil)

	m, err := unmarshalFormatInitMetadata(body)
	if err != nil {
		t.Fatal(err)
	}
	if m.FormatId.Itag != 251 || m.EndSegmentNumber != 130 {
		t.Errorf("format_id/end = %+v", m)
	}
	if m.MimeType != `audio/webm; codecs="opus"` {
		t.Errorf("mime = %q", m.MimeType)
	}
	if m.InitRange != (ByteRange{Start: 0, End: 742}) || m.IndexRange != (ByteRange{Start: 743, End: 1200}) {
		t.Errorf("ranges = init %+v index %+v", m.InitRange, m.IndexRange)
	}
	if m.DurationUnits != 1000 || m.DurationTimescale != 48000 {
		t.Errorf("duration = %d/%d", m.DurationUnits, m.DurationTimescale)
	}
}

func TestUnmarshalNextRequestPolicy(t *testing.T) {
	// target_audio_readahead_ms=1, backoff_time_ms=4, playback_cookie=7.
	body := bytes.Join([][]byte{pv(1, 20000), pv(4, 1500), pb(7, []byte("cookie-bytes"))}, nil)
	p, err := unmarshalNextRequestPolicy(body)
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetAudioReadaheadMs != 20000 || p.BackoffTimeMs != 1500 {
		t.Errorf("policy = %+v", p)
	}
	if string(p.PlaybackCookie) != "cookie-bytes" {
		t.Errorf("playback_cookie = %q", p.PlaybackCookie)
	}
}

func TestUnmarshalStreamProtectionStatus(t *testing.T) {
	body := bytes.Join([][]byte{pv(1, 3), pv(2, 5)}, nil) // status=1, max_retries=2
	s, err := unmarshalStreamProtectionStatus(body)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != 3 || s.MaxRetries != 5 {
		t.Errorf("status = %+v, want {3 5}", s)
	}
}

func TestUnmarshalSabrError(t *testing.T) {
	body := bytes.Join([][]byte{ps(1, "sabr.malformed_config"), pv(2, 17)}, nil) // type=1 (string), code=2
	s, err := unmarshalSabrError(body)
	if err != nil {
		t.Fatal(err)
	}
	if s.Type != "sabr.malformed_config" || s.Code != 17 {
		t.Errorf("error = %+v, want {sabr.malformed_config 17}", s)
	}
}

func TestUnmarshalSabrRedirect(t *testing.T) {
	body := ps(1, "https://r2---example.googlevideo.com/videoplayback?n=NEW") // url=1
	s, err := unmarshalSabrRedirect(body)
	if err != nil {
		t.Fatal(err)
	}
	if s.URL != "https://r2---example.googlevideo.com/videoplayback?n=NEW" {
		t.Errorf("url = %q", s.URL)
	}
}

func TestUnmarshalSkipsUnknownFields(t *testing.T) {
	// A MediaHeader with unknown varint (7777) and bytes (7778) fields must parse
	// the known fields and ignore the rest.
	body := bytes.Join([][]byte{
		pv(7777, 999),
		pv(3, 140), // itag
		pb(7778, []byte("future bytes field")),
		pv(9, 42), // sequence_number
	}, nil)
	h, err := unmarshalMediaHeader(body)
	if err != nil {
		t.Fatalf("unmarshal with unknown fields: %v", err)
	}
	if h.Itag != 140 || h.SequenceNumber != 42 {
		t.Errorf("known fields lost: %+v", h)
	}
}
