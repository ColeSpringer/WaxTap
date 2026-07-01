package waxtap_test

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxtap"
)

// TestFacade_CopyDownloadPreservesBytesAndFillsMetadata checks the copy path: a
// nil-ProcessSpec download is byte-identical (Transcoded false, SourceBytes ==
// OutputBytes, OutputFormat == SourceFormat), and Request.FullMetadata fills
// Result.Metadata with watch-page chapters and publish date in the same call.
func TestFacade_CopyDownloadPreservesBytesAndFillsMetadata(t *testing.T) {
	initBytes := []byte("INIT-SEG-")
	mediaBytes := []byte("MEDIA-SEGMENT-1-DATA")
	umpBody := fSabrHappyBody(initBytes, mediaBytes)

	var watchCalls int
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/player"):
			if r.Header.Get("X-Youtube-Client-Name") == "1" { // force WEB
				return resp(http.StatusOK, []byte(sabrPlayerJSON)), nil
			}
			return resp(http.StatusOK, []byte(errorPlayerJSON)), nil
		case strings.Contains(r.URL.Path, "/videoplayback"):
			return resp(http.StatusOK, umpBody), nil
		case r.URL.Path == "/watch":
			watchCalls++
			return resp(http.StatusOK, []byte(fullMetaWatchHTML)), nil
		default:
			return resp(http.StatusNotFound, nil), nil
		}
	})

	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}, POTokenProvider: fProvider{}})
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "track.webm")
	res, err := c.Download(context.Background(), waxtap.Request{
		URL:          "dummyVideo0",
		FullMetadata: true,
		ProcessSpec:  waxtap.ProcessSpec{Output: waxtap.ToFile(out), IncludeMetadata: true},
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}

	// Byte-identical copy guarantees.
	if res.Transcoded {
		t.Error("copy path must not transcode")
	}
	if res.SourceBytes == 0 || res.SourceBytes != res.OutputBytes {
		t.Errorf("SourceBytes = %d, OutputBytes = %d; want equal and non-zero", res.SourceBytes, res.OutputBytes)
	}
	if res.OutputFormat.Itag != res.SourceFormat.Itag {
		t.Errorf("OutputFormat itag %d != SourceFormat itag %d", res.OutputFormat.Itag, res.SourceFormat.Itag)
	}

	// FullMetadata ran the watch-page pass (WEB base.js discovery may also hit
	// /watch, so this is a lower bound) and filled Result.Metadata: chapters and
	// publish date come only from the watch page, so their presence proves it ran.
	if watchCalls < 1 {
		t.Errorf("watchCalls = %d, want >= 1", watchCalls)
	}
	if res.Metadata == nil {
		t.Fatal("Result.Metadata is nil; want chapters/publishDate filled")
	}
	if len(res.Metadata.Chapters) != 2 {
		t.Errorf("Metadata.Chapters = %d, want 2", len(res.Metadata.Chapters))
	}
	if got := res.Metadata.PublishDate.Format("2006-01-02"); got != "2020-01-02" {
		t.Errorf("Metadata.PublishDate = %s, want 2020-01-02", got)
	}
	if res.Metadata.Availability != waxtap.AvailabilityPublic {
		t.Errorf("Metadata.Availability = %v, want public", res.Metadata.Availability)
	}
}

// TestFacade_FullMetadataRequiresIncludeMetadata checks Request.FullMetadata is a
// no-op without IncludeMetadata: no wasted watch-page fetch and no Metadata. A
// forced ANDROID_VR client isolates the applyFullMetadata fetch (no WEB base.js
// discovery hits /watch).
func TestFacade_FullMetadataRequiresIncludeMetadata(t *testing.T) {
	umpBody := fSabrHappyBody([]byte("INIT-"), []byte("MEDIA-DATA"))
	run := func(includeMetadata bool) (*waxtap.Result, int) {
		var watchCalls int
		rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/v1/player"):
				return resp(http.StatusOK, []byte(sabrPlayerJSONFor("android"))), nil
			case strings.Contains(r.URL.Path, "/videoplayback"):
				return resp(http.StatusOK, umpBody), nil
			case r.URL.Path == "/watch":
				watchCalls++
				return resp(http.StatusOK, []byte(fullMetaWatchHTML)), nil
			default:
				return resp(http.StatusOK, nil), nil
			}
		})
		c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}, Client: "android_vr"})
		if err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "t.webm")
		res, err := c.Download(context.Background(), waxtap.Request{
			URL: "dummyVideo0", FullMetadata: true,
			ProcessSpec: waxtap.ProcessSpec{Output: waxtap.ToFile(out), IncludeMetadata: includeMetadata},
		})
		if err != nil {
			t.Fatalf("download (includeMetadata=%v): %v", includeMetadata, err)
		}
		return res, watchCalls
	}

	withMeta, watchWith := run(true)
	if watchWith != 1 {
		t.Errorf("watchCalls with IncludeMetadata = %d, want 1", watchWith)
	}
	if withMeta.Metadata == nil || withMeta.Metadata.Availability != waxtap.AvailabilityPublic {
		t.Errorf("Metadata = %+v, want Availability public", withMeta.Metadata)
	}

	noMeta, watchWithout := run(false)
	if watchWithout != 0 {
		t.Errorf("watchCalls without IncludeMetadata = %d, want 0 (the guard skips the fetch)", watchWithout)
	}
	if noMeta.Metadata != nil {
		t.Errorf("Metadata = %+v, want nil without IncludeMetadata", noMeta.Metadata)
	}
}
