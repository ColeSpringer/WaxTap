package youtube

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/waxerr"
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

// sabrTransport serves the discovery + /player traffic an Extract needs and
// replays one scripted body per SABR POST.
func sabrTransport(t *testing.T, player []byte, sabrBodies [][]byte, sabrPOSTs *int) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		if resp, ok := discoveryResp(r); ok {
			return resp, nil // WEB loads base.js for the signature timestamp
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return fixtureResp(http.StatusOK, player), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			i := *sabrPOSTs
			*sabrPOSTs++
			if i >= len(sabrBodies) {
				t.Errorf("unexpected SABR POST #%d to %s", i, r.URL)
				return fixtureResp(http.StatusInternalServerError, nil), nil
			}
			return fixtureResp(http.StatusOK, sabrBodies[i]), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}
}

func TestSABR_ResolveAndStreamBytes(t *testing.T) {
	player := readFixture(t, "player_sabr.json")
	initBytes := []byte("INIT-SEGMENT-")
	mediaBytes := []byte("MEDIA-SEGMENT-1")

	var posts int
	fp := &fakeProvider{resp: potoken.Response{Token: "QUJDREVG"}} // base64 "ABCDEF"
	c := newTestClientWith(
		sabrTransport(t, player, [][]byte{sabrHappyBody(initBytes, mediaBytes)}, &posts),
		[]ClientProfile{makeProfile(profileWeb)}, fp)

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
	if posts != 1 {
		t.Errorf("SABR POSTs = %d, want 1", posts)
	}
	// The GVS token is requested when the SABR request is built (resolution is
	// read-only and does not mint).
	if fp.gotReq.Scope != potoken.ScopeGVS {
		t.Errorf("last provider scope = %v, want GVS", fp.gotReq.Scope)
	}
}

func TestSABR_PrimedTokenReusedOnce(t *testing.T) {
	player := readFixture(t, "player_sabr.json")
	var posts, gvsMints int
	prov := providerFunc(func(req potoken.Request) (potoken.Response, error) {
		if req.Scope == potoken.ScopeGVS {
			gvsMints++
		}
		return potoken.Response{Token: "QUJDREVG"}, nil
	})
	c := newTestClientWith(
		sabrTransport(t, player, [][]byte{sabrHappyBody([]byte("INIT-"), []byte("MEDIA-"))}, &posts),
		[]ClientProfile{makeProfile(profileWeb)}, prov)

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
	if posts != 1 {
		t.Errorf("SABR POSTs = %d, want 1", posts)
	}
}

func TestSABR_OpenRefreshesOnAttestation(t *testing.T) {
	player := readFixture(t, "player_sabr.json")
	initBytes, mediaBytes := []byte("I-"), []byte("M-1")

	bodies := [][]byte{
		umpFrame(tPartStreamProtection, pbStreamProtection(3)), // ATTESTATION_REQUIRED
		sabrHappyBody(initBytes, mediaBytes),
	}
	var posts int
	rp := &recordingProvider{resp: potoken.Response{Token: "QUJDREVG"}}
	c := newTestClientWith(sabrTransport(t, player, bodies, &posts),
		[]ClientProfile{makeProfile(profileWeb)}, rp)

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
	if posts != 2 {
		t.Errorf("SABR POSTs = %d, want 2 (attestation then success)", posts)
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
	player := readFixture(t, "player_sabr.json")
	initBytes, mediaBytes := []byte("I2-"), []byte("M2-1")

	bodies := [][]byte{
		umpFrame(tPartReloadPlayer, nil), // RELOAD_PLAYER_RESPONSE
		sabrHappyBody(initBytes, mediaBytes),
	}
	var posts, playerPOSTs int
	fp := &fakeProvider{resp: potoken.Response{Token: "QUJDREVG"}}
	rt := func(r *http.Request) (*http.Response, error) {
		if resp, ok := discoveryResp(r); ok {
			return resp, nil
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			playerPOSTs++
			return fixtureResp(http.StatusOK, player), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			i := posts
			posts++
			return fixtureResp(http.StatusOK, bodies[i]), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}
	c := newTestClientWith(roundTripFunc(rt), []ClientProfile{makeProfile(profileWeb)}, fp)

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
	if posts != 2 {
		t.Errorf("SABR POSTs = %d, want 2 (reload then success)", posts)
	}
	if playerPOSTs != 2 {
		t.Errorf("/player POSTs = %d, want 2 (initial + reload re-extract)", playerPOSTs)
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
	player := readFixture(t, "player_sabr.json")
	// The reload extraction replaces the selected itag 251 with 999.
	reloadPlayer := bytes.ReplaceAll(player, []byte(`"itag": 251`), []byte(`"itag": 999`))

	var posts, playerPOSTs int
	fp := &fakeProvider{resp: potoken.Response{Token: "QUJDREVG"}}
	rt := func(r *http.Request) (*http.Response, error) {
		if resp, ok := discoveryResp(r); ok {
			return resp, nil
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			playerPOSTs++
			if playerPOSTs == 1 {
				return fixtureResp(http.StatusOK, player), nil
			}
			return fixtureResp(http.StatusOK, reloadPlayer), nil // itag 251 gone
		case strings.Contains(r.URL.Path, "/videoplayback"):
			posts++
			return fixtureResp(http.StatusOK, umpFrame(tPartReloadPlayer, nil)), nil
		}
		t.Errorf("unexpected request: %s", r.URL)
		return fixtureResp(http.StatusNotFound, nil), nil
	}
	c := newTestClientWith(roundTripFunc(rt), []ClientProfile{makeProfile(profileWeb)}, fp)

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
		t.Fatalf("Open err = %v, want ErrExtractionFailed (selected itag unavailable)", err)
	}
	if err != nil && !strings.Contains(err.Error(), "selected itag") {
		t.Errorf("err = %q, want it to mention the selected itag", err)
	}
}

func TestSABR_PrimeTokenNeedsGVSProvider(t *testing.T) {
	player := readFixture(t, "player_sabr.json")
	var posts int
	// A provider that supplies the player token but nothing for GVS.
	prov := providerFunc(func(req potoken.Request) (potoken.Response, error) {
		if req.Scope == potoken.ScopeGVS {
			return potoken.Response{}, nil
		}
		return potoken.Response{Token: "QUJDREVG"}, nil
	})
	c := newTestClientWith(sabrTransport(t, player, nil, &posts),
		[]ClientProfile{makeProfile(profileWeb)}, prov)

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
	if posts != 0 {
		t.Errorf("SABR POSTs = %d, want 0 (no GVS token, no request)", posts)
	}
}

func TestSABR_OpenUnprimedNeedsGVSProvider(t *testing.T) {
	player := readFixture(t, "player_sabr.json")
	var posts int
	prov := providerFunc(func(req potoken.Request) (potoken.Response, error) {
		if req.Scope == potoken.ScopeGVS {
			return potoken.Response{}, nil
		}
		return potoken.Response{Token: "QUJDREVG"}, nil
	})
	c := newTestClientWith(sabrTransport(t, player, nil, &posts),
		[]ClientProfile{makeProfile(profileWeb)}, prov)

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
	if posts != 0 {
		t.Errorf("SABR POSTs = %d, want 0 (no GVS token, no request)", posts)
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
			Itag: 251, LastModified: "1700000000000001", XTags: "acont=original:lang=en", ContentLength: "3500000",
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
	if cfg.Format.Itag != 251 || cfg.Format.LastModified != 1700000000000001 || cfg.Format.XTags != "acont=original:lang=en" {
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
}

// providerFunc adapts a function to potoken.Provider.
type providerFunc func(potoken.Request) (potoken.Response, error)

func (f providerFunc) ProvidePOToken(_ context.Context, req potoken.Request) (potoken.Response, error) {
	return f(req)
}
