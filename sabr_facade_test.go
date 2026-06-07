package waxtap_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"strings"
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
			// just omits sts. The watch page is never reached because WEB succeeds.
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
// media segment.
func fSabrHappyBody(initBytes, mediaBytes []byte) []byte {
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
	fmtInit := protowire.AppendVarint(protowire.AppendTag(nil, 4, protowire.VarintType), 1)

	var b []byte
	b = append(b, fUmpFrame(20, mediaHdr(1, true, 0))...)
	b = append(b, media(1, initBytes)...)
	b = append(b, fUmpFrame(20, mediaHdr(2, false, 1))...)
	b = append(b, media(2, mediaBytes)...)
	b = append(b, fUmpFrame(42, fmtInit)...)
	return b
}
