package waxtap_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/colespringer/waxtap"
	"github.com/colespringer/waxtap/potoken"
)

// sabrPlayerJSON is a minimal SABR-only /player response: URL-less audio plus a
// serverAbrStreamingUrl (no n, so descramble is a no-op) and ustreamer config.
const sabrPlayerJSON = `{
  "playabilityStatus": {"status": "OK"},
  "streamingData": {
    "expiresInSeconds": "21540",
    "serverAbrStreamingUrl": "https://r1.googlevideo.com/videoplayback?expire=9999999999",
    "adaptiveFormats": [
      {"itag": 251, "mimeType": "audio/webm; codecs=\"opus\"", "bitrate": 130000,
       "contentLength": "27", "audioQuality": "AUDIO_QUALITY_MEDIUM",
       "lastModified": "1700000000000001"}
    ]
  },
  "playerConfig": {"mediaCommonConfig": {"mediaUstreamerRequestConfig": {"videoPlaybackUstreamerConfig": "Q0FFU0FnZ0I="}}},
  "videoDetails": {"videoId": "dummyVideo0", "title": "SABR Facade Test", "lengthSeconds": "1", "author": "T"}
}`

const errorPlayerJSON = `{"playabilityStatus": {"status": "ERROR", "reason": "forced"}}`

type fProvider struct{}

func (fProvider) ProvidePOToken(_ context.Context, _ potoken.Request) (potoken.Response, error) {
	return potoken.Response{Token: "QUJDREVG"}, nil // base64 "ABCDEF"
}

func TestFacade_SABRDownloadToWriter(t *testing.T) {
	initBytes := []byte("INIT-SEG-")
	mediaBytes := []byte("MEDIA-SEGMENT-1-DATA")
	umpBody := fSabrHappyBody(initBytes, mediaBytes)

	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			// Force WEB (X-Youtube-Client-Name 1) to win; other clients fail.
			if r.Header.Get("X-Youtube-Client-Name") == "1" {
				return resp(http.StatusOK, []byte(sabrPlayerJSON)), nil
			}
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			return resp(http.StatusOK, umpBody), nil
		default:
			// Signature-timestamp discovery (embed/base.js) is best-effort; a 404
			// The request omits sts. The watch page is never reached because WEB
			// succeeds.
			return resp(http.StatusNotFound, nil), nil
		}
	})

	c, err := waxtap.New(waxtap.Options{
		HTTPClient:      &http.Client{Transport: rt},
		POTokenProvider: fProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := c.Download(context.Background(), waxtap.Request{
		URL:         "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToWriter(&buf)},
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	want := append(append([]byte{}, initBytes...), mediaBytes...)
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("downloaded bytes = %q, want %q", buf.Bytes(), want)
	}
	if res.OutputBytes != int64(len(want)) {
		t.Errorf("OutputBytes = %d, want %d", res.OutputBytes, len(want))
	}
	if res.SourceFormat.Itag != 251 {
		t.Errorf("SourceFormat.Itag = %d, want 251", res.SourceFormat.Itag)
	}
}

func TestFacade_IncompleteStreamFallsBackAcrossClients(t *testing.T) {
	initBytes := []byte("INIT-SEG-")
	mediaBytes := []byte("MEDIA-SEGMENT-1-DATA")
	happy := fSabrHappyBody(initBytes, mediaBytes) // IOS: complete in one round
	capped := fSabrBody(initBytes, mediaBytes, 2)  // ANDROID_VR: declares 2 segs, sends 1

	var androidRounds atomic.Int32
	var warned atomic.Bool
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			switch r.Header.Get("X-Youtube-Client-Name") {
			case "28": // ANDROID_VR -> SABR that caps
				return resp(http.StatusOK, []byte(sabrPlayerJSONFor("android"))), nil
			case "5": // IOS completes and also offers a higher-bitrate itag.
				return resp(http.StatusOK, []byte(sabrPlayerJSONForFmts("ios", sabrAudioFmt999+", "+sabrAudioFmt251))), nil
			default: // WEB needs a token (no provider), WEB_EMBEDDED is never reached
				return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
			}
		case strings.Contains(r.URL.RawQuery, "c=android"):
			// First round delivers a partial; later rounds deliver nothing, so the
			// stream stalls (incomplete) instead of looping forever.
			if androidRounds.Add(1) == 1 {
				return resp(http.StatusOK, capped), nil
			}
			return resp(http.StatusOK, nil), nil
		case strings.Contains(r.URL.RawQuery, "c=ios"):
			return resp(http.StatusOK, happy), nil
		default:
			return resp(http.StatusNotFound, nil), nil // sts discovery is best-effort
		}
	})

	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "track.webm")
	res, err := c.Download(context.Background(), waxtap.Request{
		URL: "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{
			Output: waxtap.ToFile(out),
			Events: func(e waxtap.Event) {
				if e.Stage == waxtap.StageWarning && e.Warning != nil && e.Warning.Code == waxtap.WarnIncompleteFallback {
					warned.Store(true)
				}
			},
		},
	})
	if err != nil {
		t.Fatalf("download should fall back to a complete client: %v", err)
	}

	want := append(append([]byte{}, initBytes...), mediaBytes...)
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, want) {
		t.Errorf("file = %q, want %q (full delivery via the fallback client)", got, want)
	}
	if res.SourceFormat.Itag != 251 {
		t.Errorf("itag = %d, want 251 (same-itag pin held across the switch)", res.SourceFormat.Itag)
	}
	if !warned.Load() {
		t.Error("expected a WarnIncompleteFallback warning when switching clients")
	}
}

// TestFacade_ColdStartWebContextFallbackWarns verifies that a failed
// player-context followed by a successful client emits one fallback warning.
func TestFacade_ColdStartWebContextFallbackWarns(t *testing.T) {
	happy := fSabrHappyBody([]byte("INIT-SEG-"), []byte("MEDIA-SEGMENT-1-DATA"))
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			if r.Header.Get("X-Youtube-Client-Name") == "28" { // ANDROID_VR delivers
				return resp(http.StatusOK, []byte(sabrPlayerJSONFor("android"))), nil
			}
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		case strings.Contains(r.URL.RawQuery, "c=android"):
			return resp(http.StatusOK, happy), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})

	// The failed provider forces the configured client chain.
	pc := potoken.PlayerContextProviderFunc(func(context.Context, string) (potoken.PlayerContext, error) {
		return potoken.PlayerContext{}, errors.New("cold start: provider not ready")
	})

	c, err := waxtap.New(waxtap.Options{
		HTTPClient:            &http.Client{Transport: rt},
		POTokenProvider:       fProvider{},
		PlayerContextProvider: pc,
	})
	if err != nil {
		t.Fatal(err)
	}

	var eventWarnings int
	var detail string
	out := filepath.Join(t.TempDir(), "track.webm")
	res, err := c.Download(context.Background(), waxtap.Request{
		URL: "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{
			Output: waxtap.ToFile(out),
			Events: func(e waxtap.Event) {
				if e.Stage == waxtap.StageWarning && e.Warning != nil && e.Warning.Code == waxtap.WarnWebContextFallback {
					eventWarnings++
					detail = e.Warning.Detail
				}
			},
		},
	})
	if err != nil {
		t.Fatalf("cold-start should fall back to android_vr: %v", err)
	}
	if res.Client != "ANDROID_VR" {
		t.Errorf("Result.Client = %q, want ANDROID_VR", res.Client)
	}
	if eventWarnings != 1 {
		t.Errorf("web-context-fallback warning events = %d, want exactly 1", eventWarnings)
	}
	// The fallback warning names the served client and includes --no-fallback, so
	// a successful download still reports that the configured player-context was
	// bypassed.
	if !strings.Contains(detail, "served via ANDROID_VR") || !strings.Contains(detail, "--no-fallback") {
		t.Errorf("warning detail = %q, want it to name the served client and the --no-fallback hint", detail)
	}
	inResult := 0
	for _, w := range res.Warnings {
		if w.Code == waxtap.WarnWebContextFallback {
			inResult++
		}
	}
	if inResult != 1 {
		t.Errorf("Result.Warnings web-context-fallback = %d, want exactly 1 (visible in --json)", inResult)
	}
}

// TestFacade_WebContextEndpointFailureSurfaced covers a configured player-context
// endpoint that fails before the fallback chain also fails. The endpoint failure
// should remain visible as a warning instead of being hidden by the final
// aggregate error.
func TestFacade_WebContextEndpointFailureSurfaced(t *testing.T) {
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil // every client unavailable
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})
	// A non-sidecar endpoint that returns an unexpected response.
	pc := potoken.PlayerContextProviderFunc(func(context.Context, string) (potoken.PlayerContext, error) {
		return potoken.PlayerContext{}, errors.New("unexpected response from non-sidecar")
	})
	c, err := waxtap.New(waxtap.Options{
		HTTPClient:            &http.Client{Transport: rt},
		POTokenProvider:       fProvider{},
		PlayerContextProvider: pc,
	})
	if err != nil {
		t.Fatal(err)
	}

	var detail string
	var warnings int
	out := filepath.Join(t.TempDir(), "track.webm")
	_, derr := c.Download(context.Background(), waxtap.Request{
		URL: "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{
			Output: waxtap.ToFile(out),
			Events: func(e waxtap.Event) {
				if e.Stage == waxtap.StageWarning && e.Warning != nil && e.Warning.Code == waxtap.WarnWebContextFallback {
					warnings++
					detail = e.Warning.Detail
				}
			},
		},
	})
	if derr == nil {
		t.Fatal("expected the exhausted chain to fail")
	}
	if warnings != 1 {
		t.Fatalf("web-context warning events = %d, want exactly 1", warnings)
	}
	if !strings.Contains(detail, "the fallback also failed") {
		t.Errorf("warning detail = %q, want it to note the endpoint failure and failed fallback", detail)
	}
}

// TestFacade_NoFallbackSuppressesEndpointWarning covers the no-fallback path. The
// web-context failure is the returned error, so the extra "fallback also failed"
// warning would be inaccurate.
func TestFacade_NoFallbackSuppressesEndpointWarning(t *testing.T) {
	rt := roundTripFn(func(_ *http.Request) (*http.Response, error) {
		return resp(http.StatusNotFound, nil), nil
	})
	pc := potoken.PlayerContextProviderFunc(func(context.Context, string) (potoken.PlayerContext, error) {
		return potoken.PlayerContext{}, errors.New("unexpected response from non-sidecar")
	})
	c, err := waxtap.New(waxtap.Options{
		HTTPClient:            &http.Client{Transport: rt},
		POTokenProvider:       fProvider{},
		PlayerContextProvider: pc,
	})
	if err != nil {
		t.Fatal(err)
	}
	var warnings int
	out := filepath.Join(t.TempDir(), "track.webm")
	_, derr := c.Download(context.Background(), waxtap.Request{
		URL:        "dummyVideo0",
		NoFallback: true,
		ProcessSpec: waxtap.ProcessSpec{
			Output: waxtap.ToFile(out),
			Events: func(e waxtap.Event) {
				if e.Stage == waxtap.StageWarning && e.Warning != nil && e.Warning.Code == waxtap.WarnWebContextFallback {
					warnings++
				}
			},
		},
	})
	if derr == nil {
		t.Fatal("expected the no-fallback request to fail")
	}
	if warnings != 0 {
		t.Errorf("web-context warnings = %d, want 0 under --no-fallback (no fallback attempted)", warnings)
	}
}

// A GVS mint failure must occur during acquisition so normal fallback remains
// available. Verify both output paths and the terminal --no-fallback case.
func TestFacade_DeadGVSProviderFallsBackToChain(t *testing.T) {
	initBytes, mediaBytes := []byte("INIT-SEG-"), []byte("MEDIA-SEGMENT-1-DATA")
	happy := fSabrHappyBody(initBytes, mediaBytes)
	want := append(append([]byte{}, initBytes...), mediaBytes...)

	newClient := func(t *testing.T) *waxtap.Client {
		rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/v1/player"):
				if r.Header.Get("X-Youtube-Client-Name") == "28" { // ANDROID_VR needs no GVS token
					return resp(http.StatusOK, []byte(sabrPlayerJSONFor("android"))), nil
				}
				return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
			case strings.Contains(r.URL.RawQuery, "c=android"):
				return resp(http.StatusOK, happy), nil
			default:
				return resp(http.StatusNotFound, nil), nil
			}
		})
		pc := potoken.PlayerContextProviderFunc(func(context.Context, string) (potoken.PlayerContext, error) {
			return iosPlayerContext(), nil // live attested context
		})
		deadPO := potoken.ProviderFunc(func(context.Context, potoken.Request) (potoken.Response, error) {
			return potoken.Response{}, nil // mints nothing usable -> ErrNeedsPOToken
		})
		c, err := waxtap.New(waxtap.Options{
			HTTPClient:            &http.Client{Transport: rt},
			PlayerContextProvider: pc,
			POTokenProvider:       deadPO,
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	webCtxCounter := func() (func(waxtap.Event), *int) {
		n := 0
		return func(e waxtap.Event) {
			if e.Stage == waxtap.StageWarning && e.Warning != nil && e.Warning.Code == waxtap.WarnWebContextFallback {
				n++
			}
		}, &n
	}

	// The file sink exercises acquireAndDownload; the writer sink exercises acquire.
	t.Run("file sink", func(t *testing.T) {
		ev, n := webCtxCounter()
		out := filepath.Join(t.TempDir(), "track.webm")
		res, err := newClient(t).Download(context.Background(), waxtap.Request{
			URL:         "dummyVideo0",
			ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out), Events: ev},
		})
		if err != nil {
			t.Fatalf("file download should fall back to android_vr: %v", err)
		}
		if res.Client != "ANDROID_VR" {
			t.Errorf("Client = %q, want ANDROID_VR", res.Client)
		}
		if got, _ := os.ReadFile(out); !bytes.Equal(got, want) {
			t.Errorf("file = %q, want %q", got, want)
		}
		if *n != 1 {
			t.Errorf("web-context-fallback warnings = %d, want exactly 1", *n)
		}
	})

	t.Run("writer sink", func(t *testing.T) {
		ev, n := webCtxCounter()
		var buf bytes.Buffer
		res, err := newClient(t).Download(context.Background(), waxtap.Request{
			URL:         "dummyVideo0",
			ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToWriter(&buf), Events: ev},
		})
		if err != nil {
			t.Fatalf("writer download should fall back to android_vr: %v", err)
		}
		if res.Client != "ANDROID_VR" {
			t.Errorf("Client = %q, want ANDROID_VR", res.Client)
		}
		if !bytes.Equal(buf.Bytes(), want) {
			t.Errorf("writer = %q, want %q", buf.Bytes(), want)
		}
		if *n != 1 {
			t.Errorf("web-context-fallback warnings = %d, want exactly 1", *n)
		}
	})

	t.Run("no-fallback hard-fails", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "track.webm")
		_, err := newClient(t).Download(context.Background(), waxtap.Request{
			URL:         "dummyVideo0",
			NoFallback:  true,
			ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out)},
		})
		if !errors.Is(err, waxtap.ErrNeedsPOToken) {
			t.Fatalf("err = %v, want ErrNeedsPOToken (exit 8) under --no-fallback", err)
		}
	})
}

// Read-only SABR resolution must not mint a delivery token.
func TestFacade_InfoResolvedSABRSkipsTokenMint(t *testing.T) {
	var gvsRequested bool
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v1/player") {
			return resp(http.StatusOK, []byte(sabrPlayerJSON)), nil // WEB -> SABR formats
		}
		return resp(http.StatusNotFound, nil), nil // sts discovery is best-effort
	})
	// Mints the player token (so extraction succeeds) but nothing for GVS.
	playerOnly := potoken.ProviderFunc(func(_ context.Context, req potoken.Request) (potoken.Response, error) {
		if req.Scope == potoken.ScopeGVS {
			gvsRequested = true
			return potoken.Response{}, nil
		}
		return potoken.Response{Token: "QUJDREVG"}, nil
	})
	c, err := waxtap.New(waxtap.Options{
		HTTPClient:      &http.Client{Transport: rt},
		Client:          "web",
		POTokenProvider: playerOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.InfoResult(context.Background(), "dummyVideo0", waxtap.InfoResolved)
	if err != nil {
		t.Fatalf("InfoResolved on a SABR format must not require a GVS token: %v", err)
	}
	if info.Video == nil || len(info.Video.Formats) == 0 {
		t.Errorf("InfoResult.Video = %+v, want resolved metadata", info.Video)
	}
	if gvsRequested {
		t.Error("read-only resolution requested a GVS token; minting should be deferred to delivery")
	}
}

// TestFacade_ForcedWebCapReportsIncompleteWithoutWatchPageRetry verifies that a
// forced WEB client does not retry the equivalent WEB watch-page path.
func TestFacade_ForcedWebCapReportsIncompleteWithoutWatchPageRetry(t *testing.T) {
	capped := fSabrBody([]byte("INIT"), []byte("MEDIA"), 2) // declares 2 segs, sends 1
	// Signature discovery may also request /watch, so detect a redundant fallback
	// through its warning rather than the request count.
	watchHTML := []byte("<html><script>var ytInitialPlayerResponse = " + sabrPlayerJSONFor("watchpage") + ";</script></html>")

	var mu sync.Mutex
	rounds := map[string]int{}
	firstRound := func(c string) bool {
		mu.Lock()
		defer mu.Unlock()
		rounds[c]++
		return rounds[c] == 1
	}

	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return resp(http.StatusOK, []byte(sabrPlayerJSONFor("web"))), nil // forced --client web
		case r.URL.Path == "/watch":
			return resp(http.StatusOK, watchHTML), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			// First round per client delivers a partial; later rounds deliver nothing,
			// so each SABR stream caps (incomplete) instead of looping forever.
			if firstRound(r.URL.Query().Get("c")) {
				return resp(http.StatusOK, capped), nil
			}
			return resp(http.StatusOK, nil), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})

	c, err := waxtap.New(waxtap.Options{
		HTTPClient:      &http.Client{Transport: rt},
		Client:          "web",
		POTokenProvider: fProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}

	var incompleteWarned atomic.Bool
	out := filepath.Join(t.TempDir(), "track.webm")
	_, derr := c.Download(context.Background(), waxtap.Request{
		URL: "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{
			Output: waxtap.ToFile(out),
			Events: func(e waxtap.Event) {
				if e.Stage == waxtap.StageWarning && e.Warning != nil && e.Warning.Code == waxtap.WarnIncompleteFallback {
					incompleteWarned.Store(true)
				}
			},
		},
	})
	if !errors.Is(derr, waxtap.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream (exit 7)", derr)
	}
	if errors.Is(derr, waxtap.ErrExtractionFailed) {
		t.Errorf("a capped forced-WEB stream must not report extraction-failed (exit 4): %v", derr)
	}
	if incompleteWarned.Load() {
		t.Error("forced WEB has no remaining clients, so no incomplete-fallback warning should fire")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Error("no output file should remain when the forced WEB stream capped")
	}
}

// TestFacade_SessionAdoptionNonWebClientWarnsDowngrade verifies that session
// adoption reports delivery by a non-WEB client.
func TestFacade_SessionAdoptionNonWebClientWarnsDowngrade(t *testing.T) {
	happy := fSabrHappyBody([]byte("INIT-SEG-"), []byte("MEDIA-SEGMENT-1-DATA"))
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			if r.Header.Get("X-Youtube-Client-Name") == "28" { // ANDROID_VR delivers
				return resp(http.StatusOK, []byte(sabrPlayerJSONFor("android"))), nil
			}
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		case strings.Contains(r.URL.RawQuery, "c=android"):
			return resp(http.StatusOK, happy), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})
	// Force a single non-WEB client while adopting a session.
	c, err := waxtap.New(waxtap.Options{
		HTTPClient: &http.Client{Transport: rt},
		Client:     "android_vr",
		Session:    &waxtap.POTokenSession{VisitorData: "CgtWSVNJVE9SXzAh"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var downgrade int
	out := filepath.Join(t.TempDir(), "track.webm")
	res, err := c.Download(context.Background(), waxtap.Request{
		URL: "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{
			Output: waxtap.ToFile(out),
			Events: func(e waxtap.Event) {
				if e.Stage == waxtap.StageWarning && e.Warning != nil &&
					e.Warning.Code == waxtap.WarnFallbackProfile && strings.Contains(e.Warning.Detail, "WEB audio") {
					downgrade++
				}
			},
		},
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if res.Client != "ANDROID_VR" {
		t.Errorf("client = %q, want ANDROID_VR", res.Client)
	}
	if downgrade != 1 {
		t.Errorf("downgrade warning events = %d, want exactly 1", downgrade)
	}
	found := false
	for _, w := range res.Warnings {
		if w.Code == waxtap.WarnFallbackProfile && strings.Contains(w.Detail, "WEB audio") {
			found = true
		}
	}
	if !found {
		t.Error("downgrade warning missing from Result.Warnings (not visible in --json)")
	}
}

// TestFacade_ForcedWebEmbeddedUnavailablePreservesEmbedHint covers an actual
// embed restriction: a PO token is present, /player returns ERROR, and the error
// still carries the embed marker used by the CLI hint.
func TestFacade_ForcedWebEmbeddedUnavailablePreservesEmbedHint(t *testing.T) {
	watchHTML := []byte(`<html><script>var ytInitialPlayerResponse = {"playabilityStatus":{"status":"ERROR","reason":"Video unavailable"}};</script></html>`)
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil // web_embedded: ERROR
		case r.URL.Path == "/watch":
			return resp(http.StatusOK, watchHTML), nil // WEB fallback also unavailable
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})
	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}, Client: "web_embedded", POTokenProvider: fProvider{}})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "track.webm")
	_, derr := c.Download(context.Background(), waxtap.Request{
		URL:         "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out)},
	})
	if !errors.Is(derr, waxtap.ErrVideoUnavailable) {
		t.Fatalf("err = %v, want ErrVideoUnavailable (exit 3)", derr)
	}
	// The CLI uses Embed to recommend switching to the WEB client.
	pe, ok := errors.AsType[*waxtap.PlayabilityError](derr)
	if !ok || !pe.Embed {
		t.Errorf("err = %v, want a PlayabilityError with Embed=true preserved", derr)
	}
}

// TestFacade_ForcedWebEmbeddedNoTokenNeedsPOToken covers forced web_embedded
// without a token provider. It should fail at token acquisition, matching forced
// WEB, rather than making a tokenless /player request and misclassifying the
// result as unavailable.
func TestFacade_ForcedWebEmbeddedNoTokenNeedsPOToken(t *testing.T) {
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})
	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}, Client: "web_embedded"})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "track.webm")
	_, derr := c.Download(context.Background(), waxtap.Request{
		URL:         "dummyVideo0",
		NoFallback:  true, // isolate the forced web_embedded slot from the watch-page fallback
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out)},
	})
	if !errors.Is(derr, waxtap.ErrNeedsPOToken) {
		t.Fatalf("err = %v, want ErrNeedsPOToken (exit 8)", derr)
	}
}

// TestFacade_InfoResultCarriesClient verifies the metadata client name.
func TestFacade_InfoResultCarriesClient(t *testing.T) {
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			if r.Header.Get("X-Youtube-Client-Name") == "28" { // ANDROID_VR leads the chain
				return resp(http.StatusOK, []byte(sabrPlayerJSONFor("android"))), nil
			}
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})
	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.InfoResult(context.Background(), "dummyVideo0", waxtap.InfoBasic)
	if err != nil {
		t.Fatal(err)
	}
	if info.Client != "ANDROID_VR" {
		t.Errorf("InfoResult.Client = %q, want ANDROID_VR", info.Client)
	}
	if info.Video == nil || info.Video.Title == "" {
		t.Errorf("InfoResult.Video = %+v, want populated metadata", info.Video)
	}
}

// TestFacade_InfoResultNoFallbackSkipsWatchPage verifies that WithNoFallback
// disables the watch-page extraction attempt on the read path. The default
// fetches the watch page (and succeeds via it); WithNoFallback does not.
func TestFacade_InfoResultNoFallbackSkipsWatchPage(t *testing.T) {
	for _, tc := range []struct {
		name        string
		opts        []waxtap.ReadOption
		wantWatched bool
		wantErr     bool
	}{
		{"default falls back to the watch page", nil, true, false},
		{"no-fallback skips the watch page", []waxtap.ReadOption{waxtap.WithNoFallback()}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			watchHTML := []byte("<html><script>var ytInitialPlayerResponse = " + sabrPlayerJSONFor("watchpage") + ";</script></html>")
			var watched atomic.Bool
			rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/v1/player"):
					return resp(http.StatusOK, []byte(errorPlayerJSON)), nil // every configured client fails
				case r.URL.Path == "/watch":
					watched.Store(true)
					return resp(http.StatusOK, watchHTML), nil
				default:
					return resp(http.StatusNotFound, nil), nil
				}
			})
			// Force ANDROID_VR (a non-cipher client) so the only thing that fetches
			// the watch page is the fallback attempt, not WEB player-JS discovery.
			c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}, Client: "android_vr"})
			if err != nil {
				t.Fatal(err)
			}
			_, infoErr := c.InfoResult(context.Background(), "dummyVideo0", waxtap.InfoBasic, tc.opts...)
			if watched.Load() != tc.wantWatched {
				t.Errorf("watch-page fetched = %v, want %v", watched.Load(), tc.wantWatched)
			}
			if (infoErr != nil) != tc.wantErr {
				t.Errorf("InfoResult err = %v, wantErr %v", infoErr, tc.wantErr)
			}
		})
	}
}

// iosPlayerContext returns a SABR context served by the test transport.
func iosPlayerContext() potoken.PlayerContext {
	return potoken.PlayerContext{
		ServerAbrURL:    "https://r1.googlevideo.com/videoplayback?expire=9999999999", // no n, descramble is a no-op
		UstreamerConfig: "Q0FFU0FnZ0I=",
		VisitorData:     "CgtWSVNJVE9SXzAh",
		ClientVersion:   "2.20260606.02.00",
		Title:           "Forced iOS via WEB context", Author: "T", LengthSeconds: 1,
		AudioFormats: []potoken.PlayerContextFormat{
			{Itag: 251, LMT: "1700000000000001", MimeType: `audio/webm; codecs="opus"`, Bitrate: 130000, AudioChannels: 2, AudioSampleRate: 48000, ContentLength: 27, ApproxDurationMs: 1000},
		},
	}
}

// TestFacade_ForcedIOSDeliversViaPlayerContext verifies that WEB player-context
// delivery takes precedence over the configured iOS client.
func TestFacade_ForcedIOSDeliversViaPlayerContext(t *testing.T) {
	initBytes, mediaBytes := []byte("INIT-SEG-"), []byte("MEDIA-SEGMENT-1-DATA")
	umpBody := fSabrHappyBody(initBytes, mediaBytes)
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "/videoplayback") {
			return resp(http.StatusOK, umpBody), nil
		}
		return resp(http.StatusNotFound, nil), nil // /player is never called when web-context delivers
	})
	pc := potoken.PlayerContextProviderFunc(func(context.Context, string) (potoken.PlayerContext, error) {
		return iosPlayerContext(), nil
	})
	c, err := waxtap.New(waxtap.Options{
		HTTPClient:            &http.Client{Transport: rt},
		Client:                "ios",
		PlayerContextProvider: pc,
		POTokenProvider:       fProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "track.webm")
	res, err := c.Download(context.Background(), waxtap.Request{
		URL:         "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out)},
	})
	if err != nil {
		t.Fatalf("forced iOS + delivering player-context should stream via WEB_CONTEXT: %v", err)
	}
	if res.Client != "WEB_CONTEXT" {
		t.Errorf("Result.Client = %q, want WEB_CONTEXT (player-context should take precedence over iOS)", res.Client)
	}
	want := append(append([]byte{}, initBytes...), mediaBytes...)
	if got, _ := os.ReadFile(out); !bytes.Equal(got, want) {
		t.Errorf("file = %q, want %q", got, want)
	}
}

// TestFacade_ForcedIOSPlayerContextFailureFallsThroughToIOSChain verifies that a
// failed player-context falls through to the configured iOS client.
func TestFacade_ForcedIOSPlayerContextFailureFallsThroughToIOSChain(t *testing.T) {
	var attemptedIOS bool
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/v1/player") || strings.Contains(r.URL.Path, "/videoplayback") {
			attemptedIOS = true // the forced iOS chain was attempted
		}
		return resp(http.StatusNotFound, nil), nil
	})
	pc := potoken.PlayerContextProviderFunc(func(context.Context, string) (potoken.PlayerContext, error) {
		return potoken.PlayerContext{}, errors.New("player-context unavailable")
	})
	c, err := waxtap.New(waxtap.Options{
		HTTPClient:            &http.Client{Transport: rt},
		Client:                "ios",
		PlayerContextProvider: pc,
		POTokenProvider:       fProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "track.webm")
	_, derr := c.Download(context.Background(), waxtap.Request{
		URL:         "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out)},
	})
	if derr == nil {
		t.Fatal("want an error from the failed iOS extraction, got nil")
	}
	if !attemptedIOS {
		t.Error("the forced iOS chain should be attempted after the player-context fails")
	}
}

func TestFacade_CreatesMissingOutputDir(t *testing.T) {
	happy := fSabrHappyBody([]byte("INIT"), []byte("MEDIA"))
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			if r.Header.Get("X-Youtube-Client-Name") == "1" { // WEB wins (has a token)
				return resp(http.StatusOK, []byte(sabrPlayerJSON)), nil
			}
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			return resp(http.StatusOK, happy), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})
	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}, POTokenProvider: fProvider{}})
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "new", "sub", "track.webm")
	if _, err := c.Download(context.Background(), waxtap.Request{
		URL:         "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out)},
	}); err != nil {
		t.Fatalf("download into a missing nested dir should create it: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Errorf("output file not written to the created dir: %v", err)
	}
}

func TestFacade_StreamReadReportsIncompleteCap(t *testing.T) {
	capped := fSabrBody([]byte("INIT"), []byte("MEDIA"), 2) // declares 2 segs, sends 1
	var rounds atomic.Int32
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			if r.Header.Get("X-Youtube-Client-Name") == "1" { // WEB wins (has a token)
				return resp(http.StatusOK, []byte(sabrPlayerJSON)), nil
			}
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			if rounds.Add(1) == 1 {
				return resp(http.StatusOK, capped), nil
			}
			return resp(http.StatusOK, nil), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})
	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}, POTokenProvider: fProvider{}})
	if err != nil {
		t.Fatal(err)
	}

	rc, _, err := c.Stream(context.Background(), waxtap.Request{URL: "dummyVideo0"})
	if err != nil {
		t.Fatalf("Stream open: %v", err)
	}
	defer rc.Close()
	if _, readErr := io.ReadAll(rc); !errors.Is(readErr, waxtap.ErrIncompleteStream) {
		t.Fatalf("read err = %v, want ErrIncompleteStream on a mid-read cap", readErr)
	}
}

// Test-only HTTP and UMP helpers.

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       readCloser(body),
		Header:     make(http.Header),
	}
}

func readCloser(b []byte) *bodyReader { return &bodyReader{Reader: bytes.NewReader(b)} }

type bodyReader struct{ *bytes.Reader }

func (bodyReader) Close() error { return nil }

func fUmpVarint(v uint64) []byte {
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

func fUmpFrame(partType int, payload []byte) []byte {
	b := fUmpVarint(uint64(partType))
	b = append(b, fUmpVarint(uint64(len(payload)))...)
	return append(b, payload...)
}

// fSabrHappyBody builds a one-round response containing an init segment and one
// media segment, declaring exactly one segment so the stream completes.
func fSabrHappyBody(initBytes, mediaBytes []byte) []byte {
	return fSabrBody(initBytes, mediaBytes, 1)
}

// fSabrBody builds a one-round response with an init segment and media segment 1,
// declaring endSeg as the final segment number. endSeg > 1 leaves later segments
// undelivered, so the stream stalls and reports an incomplete delivery.
func fSabrBody(initBytes, mediaBytes []byte, endSeg uint64) []byte {
	mediaHdr := func(id uint64, isInit bool, seq uint64) []byte {
		var b []byte
		b = protowire.AppendTag(b, 1, protowire.VarintType)
		b = protowire.AppendVarint(b, id)
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
	media := func(id uint64, data []byte) []byte {
		return fUmpFrame(21, append(fUmpVarint(id), data...))
	}
	fmtInit := protowire.AppendVarint(protowire.AppendTag(nil, 4, protowire.VarintType), endSeg)

	var b []byte
	b = append(b, fUmpFrame(20, mediaHdr(1, true, 0))...)
	b = append(b, media(1, initBytes)...)
	b = append(b, fUmpFrame(20, mediaHdr(2, false, 1))...)
	b = append(b, media(2, mediaBytes)...)
	b = append(b, fUmpFrame(42, fmtInit)...)
	return b
}

// sabrAudioFmt251 is the baseline opus itag-251 SABR audio format.
const sabrAudioFmt251 = `{"itag": 251, "mimeType": "audio/webm; codecs=\"opus\"", "bitrate": 130000,
       "contentLength": "27", "audioQuality": "AUDIO_QUALITY_MEDIUM", "lastModified": "1700000000000001"}`

// sabrAudioFmt999 is ranked above sabrAudioFmt251 without an itag pin.
const sabrAudioFmt999 = `{"itag": 999, "mimeType": "audio/webm; codecs=\"opus\"", "bitrate": 256000,
       "contentLength": "27", "audioQuality": "AUDIO_QUALITY_HIGH", "lastModified": "1700000000000002"}`

// sabrPlayerJSONFor builds a response that offers only itag 251.
func sabrPlayerJSONFor(client string) string {
	return sabrPlayerJSONForFmts(client, sabrAudioFmt251)
}

// sabrPlayerJSONForFmts builds a SABR response with an explicit format list.
func sabrPlayerJSONForFmts(client, formats string) string {
	return fmt.Sprintf(`{
  "playabilityStatus": {"status": "OK"},
  "streamingData": {
    "expiresInSeconds": "21540",
    "serverAbrStreamingUrl": "https://r1.googlevideo.com/videoplayback?expire=9999999999&c=%s",
    "adaptiveFormats": [%s]
  },
  "playerConfig": {"mediaCommonConfig": {"mediaUstreamerRequestConfig": {"videoPlaybackUstreamerConfig": "Q0FFU0FnZ0I="}}},
  "videoDetails": {"videoId": "dummyVideo0", "title": "Fallback", "lengthSeconds": "1", "author": "T"}
}`, client, formats)
}
