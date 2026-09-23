package youtube

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/colespringer/waxtap/v3/potoken"
	"github.com/colespringer/waxtap/v3/waxerr"
)

// UMP part ids and protobuf field numbers used to craft SABR responses. They
// mirror youtube/internal/sabr; duplicated here so this end-to-end test builds
// fixtures without reaching into the internal package.
const (
	tPartMediaHeader      = 20
	tPartMedia            = 21
	tPartFormatInit       = 42
	tPartReloadPlayer     = 46
	tPartStreamProtection = 58
)

// umpVarint encodes v using UMP's variable-length integer format.
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

// umpFrame wraps payload as one UMP part: varint(type) varint(size) payload.
func umpFrame(partType int, payload []byte) []byte {
	b := umpVarint(uint64(partType))
	b = append(b, umpVarint(uint64(len(payload)))...)
	return append(b, payload...)
}

// mediaFrame builds a MEDIA part: a leading header_id varint then raw bytes.
func mediaFrame(headerID uint64, media []byte) []byte {
	payload := append(umpVarint(headerID), media...)
	return umpFrame(tPartMedia, payload)
}

func umpConcat(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// pbMediaHeader encodes a MediaHeader (header_id=1, is_init_seg=8,
// sequence_number=9).
func pbMediaHeader(headerID uint64, isInit bool, seq uint64) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.VarintType)
	b = protowire.AppendVarint(b, headerID)
	if isInit {
		b = protowire.AppendTag(b, 8, protowire.VarintType)
		b = protowire.AppendVarint(b, 1)
	}
	if seq != 0 {
		b = protowire.AppendTag(b, 9, protowire.VarintType)
		b = protowire.AppendVarint(b, seq)
	}
	return b
}

// pbFormatInit encodes a FormatInitializationMetadata with end_segment_number=4.
func pbFormatInit(endSeg uint64) []byte {
	b := protowire.AppendTag(nil, 4, protowire.VarintType)
	return protowire.AppendVarint(b, endSeg)
}

// pbStreamProtection encodes a StreamProtectionStatus with status=1.
func pbStreamProtection(status uint64) []byte {
	b := protowire.AppendTag(nil, 1, protowire.VarintType)
	return protowire.AppendVarint(b, status)
}

// sabrHappyBody is a single-round SABR response: an init segment, one media
// segment (sequence 1), and a FORMAT_INITIALIZATION_METADATA marking segment 1
// as the last, so the stream completes after one round.
func sabrHappyBody(initBytes, mediaBytes []byte) []byte {
	return umpConcat(
		umpFrame(tPartMediaHeader, pbMediaHeader(1, true, 0)),
		mediaFrame(1, initBytes),
		umpFrame(tPartMediaHeader, pbMediaHeader(2, false, 1)),
		mediaFrame(2, mediaBytes),
		umpFrame(tPartFormatInit, pbFormatInit(1)),
	)
}

// sabrServer fakes the traffic a SABR test needs: the discovery and /player
// requests an Extract makes, one player per /player POST with the last
// repeating, and one scripted body per SABR POST. It records each SABR request
// body so a test can read the FormatId it named.
type sabrServer struct {
	t           *testing.T
	players     [][]byte
	bodies      [][]byte
	playerPOSTs int
	reqs        [][]byte
}

func (s *sabrServer) RoundTrip(r *http.Request) (*http.Response, error) {
	if resp, ok := discoveryResp(r); ok {
		return resp, nil // WEB loads base.js for the signature timestamp
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/v1/player"):
		i := min(s.playerPOSTs, len(s.players)-1)
		s.playerPOSTs++
		return fixtureResp(http.StatusOK, s.players[i]), nil
	case strings.Contains(r.URL.Path, "/videoplayback"):
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.t.Errorf("read SABR request: %v", err)
		}
		i := len(s.reqs)
		s.reqs = append(s.reqs, body)
		if i >= len(s.bodies) {
			s.t.Errorf("unexpected SABR POST #%d to %s", i, r.URL)
			return fixtureResp(http.StatusInternalServerError, nil), nil
		}
		return fixtureResp(http.StatusOK, s.bodies[i]), nil
	}
	s.t.Errorf("unexpected request: %s", r.URL)
	return fixtureResp(http.StatusNotFound, nil), nil
}

func TestSABR_ResolveAndStreamBytes(t *testing.T) {
	initBytes := []byte("INIT-SEGMENT-")
	mediaBytes := []byte("MEDIA-SEGMENT-1")

	srv := &sabrServer{t: t, players: [][]byte{readFixture(t, "player_sabr.json")}, bodies: [][]byte{sabrHappyBody(initBytes, mediaBytes)}}
	fp := &fakeProvider{resp: potoken.Response{Token: "QUJDREVG"}} // base64 "ABCDEF"
	c := newTestClientWith(srv, []ClientProfile{makeProfile(profileWeb)}, fp)

	ext, err := c.Extract(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	plan, err := c.Resolve(context.Background(), ext, 0) // index 0 = opus 251
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.SABR == nil {
		t.Fatalf("MediaPlan.SABR = nil, want a SABR stream (URL-less formats)")
	}
	if plan.Direct != nil {
		t.Errorf("MediaPlan.Direct = %+v, want nil", plan.Direct)
	}

	rc, info, err := plan.SABR.Open(context.Background(), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := append(append([]byte{}, initBytes...), mediaBytes...)
	if !bytes.Equal(got, want) {
		t.Errorf("streamed bytes = %q, want %q", got, want)
	}
	if info.ContentLength != 3500000 {
		t.Errorf("ContentLength = %d, want 3500000 (from the player response)", info.ContentLength)
	}
	if len(srv.reqs) != 1 {
		t.Errorf("SABR POSTs = %d, want 1", len(srv.reqs))
	}
	// The GVS token is requested when the SABR request is built (resolution is
	// read-only and does not mint).
	if fp.gotReq.Scope != potoken.ScopeGVS {
		t.Errorf("last provider scope = %v, want GVS", fp.gotReq.Scope)
	}
}

func TestSABR_PrimedTokenReusedOnce(t *testing.T) {
	var gvsMints int
	prov := providerFunc(func(req potoken.Request) (potoken.Response, error) {
		if req.Scope == potoken.ScopeGVS {
			gvsMints++
		}
		return potoken.Response{Token: "QUJDREVG"}, nil
	})
	srv := &sabrServer{t: t, players: [][]byte{readFixture(t, "player_sabr.json")}, bodies: [][]byte{sabrHappyBody([]byte("INIT-"), []byte("MEDIA-"))}}
	c := newTestClientWith(srv, []ClientProfile{makeProfile(profileWeb)}, prov)

	ext, err := c.Extract(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	plan, err := c.Resolve(context.Background(), ext, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if gvsMints != 0 {
		t.Fatalf("GVS mints after resolve = %d, want 0 (resolution is read-only)", gvsMints)
	}
	if err := plan.SABR.PrimeToken(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if gvsMints != 1 {
		t.Fatalf("GVS mints after prime = %d, want 1", gvsMints)
	}
	rc, _, err := plan.SABR.Open(context.Background(), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if gvsMints != 1 {
		t.Errorf("GVS mints after open = %d, want 1 (reused primed token, no second mint)", gvsMints)
	}
	if len(srv.reqs) != 1 {
		t.Errorf("SABR POSTs = %d, want 1", len(srv.reqs))
	}
}

func TestSABR_OpenRefreshesOnAttestation(t *testing.T) {
	initBytes, mediaBytes := []byte("I-"), []byte("M-1")

	srv := &sabrServer{t: t, players: [][]byte{readFixture(t, "player_sabr.json")}, bodies: [][]byte{
		umpFrame(tPartStreamProtection, pbStreamProtection(3)), // ATTESTATION_REQUIRED
		sabrHappyBody(initBytes, mediaBytes),
	}}
	rp := &recordingProvider{resp: potoken.Response{Token: "QUJDREVG"}}
	c := newTestClientWith(srv, []ClientProfile{makeProfile(profileWeb)}, rp)

	ext, err := c.Extract(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	plan, err := c.Resolve(context.Background(), ext, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rc, _, err := plan.SABR.Open(context.Background(), nil)
	if err != nil {
		t.Fatalf("open should retry after attestation: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if want := append(append([]byte{}, initBytes...), mediaBytes...); !bytes.Equal(got, want) {
		t.Errorf("streamed bytes = %q, want %q", got, want)
	}
	if len(srv.reqs) != 2 {
		t.Errorf("SABR POSTs = %d, want 2 (attestation then success)", len(srv.reqs))
	}

	var gvs []potoken.Request
	for _, r := range rp.reqs {
		if r.Scope == potoken.ScopeGVS {
			gvs = append(gvs, r)
		}
	}
	if len(gvs) != 2 {
		t.Fatalf("GVS token requests = %d, want 2 (initial + attestation refresh)", len(gvs))
	}
	if gvs[0].Failure != nil {
		t.Errorf("first GVS request should carry no failure, got %+v", gvs[0].Failure)
	}
	if gvs[1].Failure == nil {
		t.Error("attestation refresh GVS request should carry a failure hint")
	}
}

func TestSABR_OpenReextractsOnReload(t *testing.T) {
	initBytes, mediaBytes := []byte("I2-"), []byte("M2-1")

	srv := &sabrServer{t: t, players: [][]byte{readFixture(t, "player_sabr.json")}, bodies: [][]byte{
		umpFrame(tPartReloadPlayer, nil), // RELOAD_PLAYER_RESPONSE
		sabrHappyBody(initBytes, mediaBytes),
	}}
	fp := &fakeProvider{resp: potoken.Response{Token: "QUJDREVG"}}
	c := newTestClientWith(srv, []ClientProfile{makeProfile(profileWeb)}, fp)

	ext, err := c.Extract(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	plan, err := c.Resolve(context.Background(), ext, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rc, _, err := plan.SABR.Open(context.Background(), nil)
	if err != nil {
		t.Fatalf("open should retry after reload: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if want := append(append([]byte{}, initBytes...), mediaBytes...); !bytes.Equal(got, want) {
		t.Errorf("streamed bytes = %q, want %q", got, want)
	}
	if len(srv.reqs) != 2 {
		t.Errorf("SABR POSTs = %d, want 2 (reload then success)", len(srv.reqs))
	}
	if srv.playerPOSTs != 2 {
		t.Errorf("/player POSTs = %d, want 2 (initial + reload re-extract)", srv.playerPOSTs)
	}
}

func TestSABR_OpenRefreshesOnHTTP403(t *testing.T) {
	player := readFixture(t, "player_sabr.json")
	initBytes, mediaBytes := []byte("I3-"), []byte("M3-1")

	var posts int
	rp := &recordingProvider{resp: potoken.Response{Token: "QUJDREVG"}}
	rt := func(r *http.Request) (*http.Response, error) {
		if resp, ok := discoveryResp(r); ok {
			return resp, nil
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return fixtureResp(http.StatusOK, player), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			i := posts
			posts++
			if i == 0 {
				return fixtureResp(http.StatusForbidden, nil), nil // token rejected at HTTP layer
			}
			return fixtureResp(http.StatusOK, sabrHappyBody(initBytes, mediaBytes)), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}
	c := newTestClientWith(roundTripFunc(rt), []ClientProfile{makeProfile(profileWeb)}, rp)

	ext, err := c.Extract(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	plan, err := c.Resolve(context.Background(), ext, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rc, _, err := plan.SABR.Open(context.Background(), nil)
	if err != nil {
		t.Fatalf("open should retry after HTTP 403: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if want := append(append([]byte{}, initBytes...), mediaBytes...); !bytes.Equal(got, want) {
		t.Errorf("streamed bytes = %q, want %q", got, want)
	}
	if posts != 2 {
		t.Errorf("SABR POSTs = %d, want 2 (403 then success)", posts)
	}

	var gvs []potoken.Request
	for _, r := range rp.reqs {
		if r.Scope == potoken.ScopeGVS {
			gvs = append(gvs, r)
		}
	}
	if len(gvs) != 2 {
		t.Fatalf("GVS token requests = %d, want 2 (initial + 403 refresh)", len(gvs))
	}
	if gvs[1].Failure == nil || gvs[1].Failure.StatusCode != http.StatusForbidden {
		t.Errorf("refresh GVS failure = %+v, want a hint with StatusCode 403", gvs[1].Failure)
	}
}

func TestSABR_ReloadErrorsWhenItagGone(t *testing.T) {
	base := readFixture(t, "player_sabr.json")
	for _, tc := range []struct {
		name     string
		lmt      string // the selected rendition's lastModified in both responses
		old, new string // what the reload response changes
	}{
		// The reload extraction replaces the selected itag 251 with 999.
		{"itag gone", "1700000000000001", `"itag": 251`, `"itag": 999`},
		// Itag 251 stays but now names a dub, so the selected rendition is gone.
		{"xtags changed", "1700000000000001", xtagsOriginalEn, xtagsDubbedAutoDe},
		// A non-numeric lmt reaches the wire as 0; the refusal names the lmt the
		// response spelled.
		{"xtags changed, non-numeric lmt", "17000000000000x1", xtagsOriginalEn, xtagsDubbedAutoDe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			player := bytes.ReplaceAll(base, []byte(`"lastModified": "1700000000000001"`), []byte(`"lastModified": "`+tc.lmt+`"`))
			reloadPlayer := bytes.ReplaceAll(player, []byte(tc.old), []byte(tc.new))
			if bytes.Equal(reloadPlayer, player) {
				t.Fatalf("%q not found in the fixture", tc.old)
			}

			// Enough reloads for the reload limit, so a wrong re-selection fails
			// on the assertions below rather than on the fake.
			reload := umpFrame(tPartReloadPlayer, nil)
			srv := &sabrServer{t: t, players: [][]byte{player, reloadPlayer}, bodies: [][]byte{reload, reload, reload}}
			fp := &fakeProvider{resp: potoken.Response{Token: "QUJDREVG"}}
			c := newTestClientWith(srv, []ClientProfile{makeProfile(profileWeb)}, fp)

			ext, err := c.Extract(context.Background(), "dummyVideo0")
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			plan, err := c.Resolve(context.Background(), ext, 0)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			_, _, err = plan.SABR.Open(context.Background(), nil)
			if !errors.Is(err, waxerr.ErrExtractionFailed) {
				t.Fatalf("Open err = %v, want ErrExtractionFailed (selected rendition unavailable)", err)
			}
			if !strings.Contains(err.Error(), "selected itag") || !strings.Contains(err.Error(), tc.lmt) {
				t.Errorf("err = %q, want it to name the selected itag and lmt", err)
			}
			if len(srv.reqs) != 1 {
				t.Errorf("SABR POSTs = %d, want 1 (refused at the first reload)", len(srv.reqs))
			}
		})
	}
}

func TestFindFormat(t *testing.T) {
	yes := true
	want := rawFormat{Itag: 251, LastModified: "1700000000000001", XTags: xtagsOriginalEn}
	decoy := rawFormat{Itag: 251, LastModified: "1700000000000003", XTags: xtagsOriginalDRC, IsDrc: &yes}
	vb := rawFormat{Itag: 251, LastModified: "1700000000000004", XTags: xtagsOriginalVB}
	aac := rawFormat{Itag: 140, LastModified: "1700000000000002"}
	reencoded := want
	reencoded.LastModified = "1700000000000009"
	dub := want
	dub.XTags = xtagsDubbedAutoDe
	drc := reencoded
	drc.IsDrc = &yes
	otherTrack := reencoded
	otherTrack.AudioTrack = &rawAudioTrack{ID: "fr.3"}
	emptyTrack := reencoded
	emptyTrack.AudioTrack = &rawAudioTrack{}
	exactDRC := want
	exactDRC.IsDrc = &yes
	exactTrack := want
	exactTrack.AudioTrack = &rawAudioTrack{ID: "fr.3"}
	unmarked := reencoded // a DRC variant neither xtags nor isDrc marks
	unmarked.LastModified = "1700000000000005"

	cases := []struct {
		name      string
		raw       []rawFormat
		wantIdx   int
		wantExact bool
	}{
		{"exact behind decoys", []rawFormat{decoy, vb, want, aac}, 2, true},
		{"re-encoded", []rawFormat{decoy, vb, reencoded, aac}, 2, false},
		{"same itag and lmt, other xtags", []rawFormat{dub, aac}, -1, false},
		{"same itag and xtags, other DRC flag", []rawFormat{drc, aac}, -1, false},
		{"same itag and xtags, other track", []rawFormat{otherTrack, aac}, -1, false},
		{"no track and empty track id alike", []rawFormat{emptyTrack}, 0, false},
		// The triple is the encoding: a DRC flag or track the response adds or
		// drops does not make it another one.
		{"exact triple, other DRC flag", []rawFormat{exactDRC, aac}, 0, true},
		{"exact triple, other track", []rawFormat{exactTrack, aac}, 0, true},
		// Two entries fit the rendition at new lmts, one of them a DRC variant
		// nothing marks: refuse rather than guess.
		{"two re-encodes", []rawFormat{unmarked, reencoded, aac}, -1, false},
		{"empty list", nil, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, exact := findFormat(tc.raw, want)
			if idx != tc.wantIdx || exact != tc.wantExact {
				t.Errorf("findFormat = (%d, %v), want (%d, %v)", idx, exact, tc.wantIdx, tc.wantExact)
			}
		})
	}

	// Two different non-numeric lmts both reach the wire as 0; as spelled they
	// are two encodings, so the second one is a re-encode.
	odd, odder := want, want
	odd.LastModified, odder.LastModified = "17000000000000x1", "17000000000000y1"
	if idx, exact := findFormat([]rawFormat{odder}, odd); idx != 0 || exact {
		t.Errorf("findFormat(non-numeric lmts) = (%d, %v), want (0, false)", idx, exact)
	}
}

// TestSABRStreamZeroValue holds a zero SABRStream, which a caller can build
// since the type is exported: Format and Diagnostic report nothing rather than
// panic.
func TestSABRStreamZeroValue(t *testing.T) {
	s := &SABRStream{}
	if f := s.Format(); f.Itag != 0 || f.ContentLength != 0 {
		t.Errorf("Format() = %+v, want no identity or size", f)
	}
	if d := (MediaPlan{SABR: s}).Diagnostic(); !d.IsSABR || d.ContentLength != 0 || !d.ExpiresAt.IsZero() {
		t.Errorf("Diagnostic() = %+v, want IsSABR with no size or expiry", d)
	}
}

func TestExtractionFindEncoding(t *testing.T) {
	yes := true
	want := rawFormat{Itag: 251, LastModified: "1700000000000001", XTags: xtagsOriginalEn}
	prev := &Extraction{rawAudio: []rawFormat{{Itag: 140, LastModified: "1700000000000002"}, want}}
	drc := rawFormat{Itag: 251, LastModified: "1700000000000003", XTags: xtagsOriginalDRC, IsDrc: &yes}
	reencoded := want
	reencoded.LastModified = "1700000000000009"
	dub := want
	dub.XTags = xtagsDubbedAutoDe

	cases := []struct {
		name    string
		raw     []rawFormat
		prevIdx int
		wantIdx int
		wantOK  bool
	}{
		{"same encoding at a new index", []rawFormat{drc, want}, 1, 1, true},
		// A resumed byte range cannot move to a re-encode, unlike a SABR reload.
		{"re-encoded", []rawFormat{drc, reencoded}, 1, -1, false},
		{"same lmt, other xtags", []rawFormat{dub}, 1, -1, false},
		{"index out of range", []rawFormat{want}, 2, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, ok := (&Extraction{rawAudio: tc.raw}).FindEncoding(prev, tc.prevIdx)
			if idx != tc.wantIdx || ok != tc.wantOK {
				t.Errorf("FindEncoding = (%d, %v), want (%d, %v)", idx, ok, tc.wantIdx, tc.wantOK)
			}
		})
	}

	// The triple is the encoding: a re-fetch that drops the track still names it,
	// so the byte range resumes rather than restarting from zero.
	tracked := want
	tracked.AudioTrack = &rawAudioTrack{ID: "en.4"}
	prevTracked := &Extraction{rawAudio: []rawFormat{tracked}}
	if idx, ok := (&Extraction{rawAudio: []rawFormat{drc, want}}).FindEncoding(prevTracked, 0); idx != 1 || !ok {
		t.Errorf("FindEncoding(track dropped) = (%d, %v), want (1, true)", idx, ok)
	}
}

// pbField returns the first occurrence of field num in one message level: a
// varint's value or a length-delimited field's bytes. ok is false when absent.
func pbField(t *testing.T, b []byte, num protowire.Number) (v uint64, raw []byte, ok bool) {
	t.Helper()
	for len(b) > 0 {
		n, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			t.Fatalf("bad protobuf tag: %v", protowire.ParseError(l))
		}
		b = b[l:]
		var fv uint64
		var fb []byte
		switch typ {
		case protowire.VarintType:
			fv, l = protowire.ConsumeVarint(b)
		case protowire.BytesType:
			fb, l = protowire.ConsumeBytes(b)
		default:
			l = protowire.ConsumeFieldValue(n, typ, b)
		}
		if l < 0 {
			t.Fatalf("bad protobuf field %d: %v", n, protowire.ParseError(l))
		}
		b = b[l:]
		if n == num {
			return fv, fb, true
		}
	}
	return 0, nil, false
}

// requestFormatID reads the FormatId a captured SABR request prefers:
// preferred_audio_format_ids (16), with itag (1), last_modified (2), xtags (3).
func requestFormatID(t *testing.T, body []byte) (itag, lmt uint64, xtags string) {
	t.Helper()
	_, fid, ok := pbField(t, body, 16)
	if !ok {
		t.Fatal("SABR request carries no preferred_audio_format_ids")
	}
	itag, _, _ = pbField(t, fid, 1)
	lmt, _, _ = pbField(t, fid, 2)
	_, x, _ := pbField(t, fid, 3)
	return itag, lmt, string(x)
}

// requestDRCEnabled reports whether a captured SABR request sets
// client_abr_state (1) drc_enabled (46).
func requestDRCEnabled(t *testing.T, body []byte) bool {
	t.Helper()
	_, cas, _ := pbField(t, body, 1)
	_, _, ok := pbField(t, cas, 46)
	return ok
}

// TestSABR_ReloadRepinsTheRendition reloads onto a response that lists the
// selected rendition behind two decoys under the same itag and acont: its DRC
// variant and a full-range variant with other xtags (vb=1). Only the (itag,
// lmt, xtags) triple finds it, and only a re-encode of that rendition logs.
func TestSABR_ReloadRepinsTheRendition(t *testing.T) {
	base := readFixture(t, "player_sabr.json")
	reload := readFixture(t, "player_sabr_reload.json")
	reencoded := bytes.ReplaceAll(reload, []byte(`"lastModified": "1700000000000001"`), []byte(`"lastModified": "1700000000000009"`))
	if bytes.Equal(reencoded, reload) {
		t.Fatal("selected lastModified not found in the reload fixture")
	}
	again := umpFrame(tPartReloadPlayer, nil)
	happy := sabrHappyBody([]byte("I4-"), []byte("M4-1"))

	cases := []struct {
		name     string
		players  [][]byte
		bodies   [][]byte
		wantLMTs []uint64 // the lmt each post-reload request names
		wantLogs int      // re-encode log records
	}{
		{"exact triple", [][]byte{base, reload}, [][]byte{again, happy}, []uint64{1700000000000001}, 0},
		{"re-encoded", [][]byte{base, reencoded}, [][]byte{again, happy}, []uint64{1700000000000009}, 1},
		// The second response no longer carries the original lmt, so a reload
		// that compared against the original request would log the re-encode
		// twice.
		{"second reload keeps the first pick", [][]byte{base, reencoded}, [][]byte{again, again, happy}, []uint64{1700000000000009, 1700000000000009}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &sabrServer{t: t, players: tc.players, bodies: tc.bodies}
			h := &levelCaptureHandler{match: "re-encoded the selected format"}
			c := New(Config{
				HTTP:            fastTransport(srv),
				Profiles:        []ClientProfile{makeProfile(profileWeb)},
				POTokenProvider: &fakeProvider{resp: potoken.Response{Token: "QUJDREVG"}},
				Logger:          slog.New(h),
			})

			ext, err := c.Extract(context.Background(), "dummyVideo0")
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			plan, err := c.Resolve(context.Background(), ext, 0)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			rc, _, err := plan.SABR.Open(context.Background(), nil)
			if err != nil {
				t.Fatalf("open should retry after reload: %v", err)
			}
			defer rc.Close()
			if got, _ := io.ReadAll(rc); string(got) != "I4-M4-1" {
				t.Errorf("streamed bytes = %q, want %q", got, "I4-M4-1")
			}
			if plan.SABR.formatIdx != 2 {
				t.Errorf("formatIdx = %d, want 2 (the selected rendition, behind both decoys)", plan.SABR.formatIdx)
			}
			if len(srv.reqs) != len(tc.wantLMTs)+1 {
				t.Fatalf("SABR POSTs = %d, want %d", len(srv.reqs), len(tc.wantLMTs)+1)
			}
			for i, want := range tc.wantLMTs {
				req := srv.reqs[i+1]
				if itag, lmt, xtags := requestFormatID(t, req); itag != 251 || lmt != want || xtags != xtagsOriginalEn {
					t.Errorf("request %d FormatId = (%d, %d, %q), want (251, %d, %q)", i+2, itag, lmt, xtags, want, xtagsOriginalEn)
				}
				if requestDRCEnabled(t, req) {
					t.Errorf("request %d sets drc_enabled, want the full-range rendition", i+2)
				}
			}
			if len(h.levels) != tc.wantLogs {
				t.Errorf("re-encode log records = %v, want %d", h.levels, tc.wantLogs)
			}
			for _, l := range h.levels {
				if l != slog.LevelInfo {
					t.Errorf("re-encode log level = %v, want Info", l)
				}
			}
		})
	}
}

// TestSABR_WebContextReloadRepinsTriple is the reload on the WEB path: the
// fresh context lists the selected rendition behind the same two decoys, and
// the stream keeps it under the new context's generation.
func TestSABR_WebContextReloadRepinsTriple(t *testing.T) {
	selected := sampleContext().AudioFormats[0] // itag 251, lmt 1719185012384481
	selected.XTags = xtagsOriginalEn
	drc := selected
	drc.LMT, drc.XTags, drc.IsDrc = "1719185012384483", xtagsOriginalDRC, true
	vb := selected
	vb.LMT, vb.XTags = "1719185012384484", xtagsOriginalVB

	var calls int
	pcp := potoken.PlayerContextProviderFunc(func(context.Context, string) (potoken.PlayerContext, error) {
		calls++
		pc := sampleContext()
		pc.ServerAbrURL = "https://rr3.googlevideo.com/videoplayback?expire=1781138473&sabr=1" // no n to descramble
		aac := pc.AudioFormats[1]
		pc.AudioFormats, pc.Generation = []potoken.PlayerContextFormat{selected, aac}, 1
		if calls > 1 {
			pc.AudioFormats, pc.Generation = []potoken.PlayerContextFormat{drc, vb, selected, aac}, 2
		}
		return pc, nil
	})
	bodies := [][]byte{umpFrame(tPartReloadPlayer, nil), sabrHappyBody([]byte("I6-"), []byte("M6-1"))}
	var reqs [][]byte
	rt := func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.URL.Path, "/videoplayback") {
			t.Errorf("unexpected request: %s", r.URL)
			return fixtureResp(http.StatusNotFound, nil), nil
		}
		body, _ := io.ReadAll(r.Body)
		reqs = append(reqs, body)
		if len(reqs) > len(bodies) {
			t.Errorf("unexpected SABR POST #%d", len(reqs)-1)
			return fixtureResp(http.StatusInternalServerError, nil), nil
		}
		return fixtureResp(http.StatusOK, bodies[len(reqs)-1]), nil
	}
	// fakeResolver offers no player inspection, so nothing fetches base.js.
	c := New(Config{
		HTTP:                  fastTransport(roundTripFunc(rt)),
		Resolver:              &fakeResolver{},
		POTokenProvider:       &fakeProvider{resp: potoken.Response{Token: "QUJDREVG"}},
		PlayerContextProvider: pcp,
	})

	ext, err := c.ExtractWebContext(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("ExtractWebContext: %v", err)
	}
	plan, err := c.Resolve(context.Background(), ext, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rc, _, err := plan.SABR.Open(context.Background(), nil)
	if err != nil {
		t.Fatalf("open should retry after reload: %v", err)
	}
	rc.Close()
	if calls != 2 {
		t.Errorf("player-context calls = %d, want 2 (initial + reload)", calls)
	}
	if plan.SABR.formatIdx != 2 {
		t.Errorf("formatIdx = %d, want 2 (the selected rendition, behind both decoys)", plan.SABR.formatIdx)
	}
	if len(reqs) != 2 {
		t.Fatalf("SABR POSTs = %d, want 2 (reload then success)", len(reqs))
	}
	if itag, lmt, xtags := requestFormatID(t, reqs[1]); itag != 251 || lmt != 1719185012384481 || xtags != xtagsOriginalEn {
		t.Errorf("post-reload FormatId = (%d, %d, %q), want (251, 1719185012384481, %q)", itag, lmt, xtags, xtagsOriginalEn)
	}
	if gen := plan.SABR.ContextGeneration(); gen != 2 {
		t.Errorf("ContextGeneration = %d, want 2 (the context that streamed)", gen)
	}
}

func TestSABR_PrimeTokenNeedsGVSProvider(t *testing.T) {
	srv := &sabrServer{t: t, players: [][]byte{readFixture(t, "player_sabr.json")}}
	// A provider that supplies the player token but nothing for GVS.
	prov := providerFunc(func(req potoken.Request) (potoken.Response, error) {
		if req.Scope == potoken.ScopeGVS {
			return potoken.Response{}, nil
		}
		return potoken.Response{Token: "QUJDREVG"}, nil
	})
	c := newTestClientWith(srv, []ClientProfile{makeProfile(profileWeb)}, prov)

	ext, err := c.Extract(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	// Resolution is read-only and must not mint a GVS token.
	plan, err := c.Resolve(context.Background(), ext, 0)
	if err != nil {
		t.Fatalf("Resolve should not mint a token: %v", err)
	}
	// PrimeToken surfaces the missing token before delivery starts.
	if err := plan.SABR.PrimeToken(context.Background()); !errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Fatalf("PrimeToken err = %v, want ErrNeedsPOToken", err)
	}
	if len(srv.reqs) != 0 {
		t.Errorf("SABR POSTs = %d, want 0 (no GVS token, no request)", len(srv.reqs))
	}
}

func TestSABR_OpenUnprimedNeedsGVSProvider(t *testing.T) {
	srv := &sabrServer{t: t, players: [][]byte{readFixture(t, "player_sabr.json")}}
	prov := providerFunc(func(req potoken.Request) (potoken.Response, error) {
		if req.Scope == potoken.ScopeGVS {
			return potoken.Response{}, nil
		}
		return potoken.Response{Token: "QUJDREVG"}, nil
	})
	c := newTestClientWith(srv, []ClientProfile{makeProfile(profileWeb)}, prov)

	ext, err := c.Extract(context.Background(), "dummyVideo0")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	plan, err := c.Resolve(context.Background(), ext, 0)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, _, err := plan.SABR.Open(context.Background(), nil); !errors.Is(err, waxerr.ErrNeedsPOToken) {
		t.Fatalf("Open (unprimed) err = %v, want ErrNeedsPOToken", err)
	}
	if len(srv.reqs) != 0 {
		t.Errorf("SABR POSTs = %d, want 0 (no GVS token, no request)", len(srv.reqs))
	}
}

func TestBuildSABRConfig(t *testing.T) {
	c := New(Config{Resolver: &fakeResolver{}, POTokenProvider: &fakeProvider{resp: potoken.Response{Token: "QUJDREVG"}}})

	sess := newSession("US")
	sess.visitorData = "VIS"
	ext := &Extraction{
		video:   &Video{ID: "dummyVideo0"},
		profile: makeProfile(profileWeb),
		session: sess,
		rawAudio: []rawFormat{{
			Itag: 251, LastModified: "1700000000000001", XTags: xtagsOriginalEn, ContentLength: "3500000",
		}},
		// No n parameter, so descramble is a no-op and needs no base.js fetch.
		serverAbrURL:    "https://r1.googlevideo.com/videoplayback?expire=9999999999",
		ustreamerConfig: "Q0FFU0FnZ0I=",
	}

	cfg, err := c.buildSABRConfig(context.Background(), ext, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerAbrURL != ext.serverAbrURL {
		t.Errorf("ServerAbrURL = %q, want %q (unchanged, no n)", cfg.ServerAbrURL, ext.serverAbrURL)
	}
	if string(cfg.POToken) != "ABCDEF" {
		t.Errorf("POToken = %q, want decoded \"ABCDEF\"", cfg.POToken)
	}
	if len(cfg.UstreamerConfig) == 0 {
		t.Error("UstreamerConfig should be base64-decoded to bytes")
	}
	if cfg.Format.Itag != 251 || cfg.Format.LastModified != 1700000000000001 || cfg.Format.XTags != xtagsOriginalEn {
		t.Errorf("Format = %+v, want itag 251 with lastModified/xtags", cfg.Format)
	}
	if cfg.ClientInfo.ClientName != int32(profileWeb.InnerTubeID) || cfg.ClientInfo.ClientVersion != profileWeb.Version {
		t.Errorf("ClientInfo = %+v, want WEB identity", cfg.ClientInfo)
	}
	if cfg.UserAgent != ext.profile.UserAgent {
		t.Errorf("UserAgent = %q, want %q", cfg.UserAgent, ext.profile.UserAgent)
	}
	if cfg.ContentLength != 3500000 {
		t.Errorf("ContentLength = %d, want 3500000", cfg.ContentLength)
	}
	if cfg.DRC || cfg.AudioTrackID != "" {
		t.Errorf("DRC = %v, AudioTrackID = %q, want false and empty for the default full-range track", cfg.DRC, cfg.AudioTrackID)
	}

	// A DRC rendition of a named track declares both in client_abr_state.
	yes := true
	ext.rawAudio[0].IsDrc = &yes
	ext.rawAudio[0].AudioTrack = &rawAudioTrack{ID: "en.4"}
	cfg, err = c.buildSABRConfig(context.Background(), ext, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DRC || cfg.AudioTrackID != "en.4" {
		t.Errorf("DRC = %v, AudioTrackID = %q, want true and en.4", cfg.DRC, cfg.AudioTrackID)
	}
}

// providerFunc adapts a function to potoken.Provider.
type providerFunc func(potoken.Request) (potoken.Response, error)

func (f providerFunc) ProvidePOToken(_ context.Context, req potoken.Request) (potoken.Response, error) {
	return f(req)
}
