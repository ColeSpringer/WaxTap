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

func TestFacade_AllClientsCapReportsIncomplete(t *testing.T) {
	capped := fSabrBody([]byte("INIT"), []byte("MEDIA"), 2) // declares 2 segs, sends 1
	// The watch-page fallback extracts its own (capping) SABR context from the page.
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

	out := filepath.Join(t.TempDir(), "track.webm")
	_, derr := c.Download(context.Background(), waxtap.Request{
		URL:         "dummyVideo0",
		ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out)},
	})
	if !errors.Is(derr, waxtap.ErrIncompleteStream) {
		t.Fatalf("err = %v, want ErrIncompleteStream (exit 7)", derr)
	}
	if errors.Is(derr, waxtap.ErrExtractionFailed) {
		t.Errorf("an all-capped chain must not report extraction-failed (exit 4): %v", derr)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Error("no output file should remain when every client capped")
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
