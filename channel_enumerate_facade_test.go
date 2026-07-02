package waxtap_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/v2"
)

// minimalBrowse is a one-entry playlist browse response with no continuation.
const minimalBrowse = `{"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[{"tabRenderer":{"content":{"sectionListRenderer":{"contents":[{"itemSectionRenderer":{"contents":[{"playlistVideoRenderer":{"videoId":"vidvidvid00","title":{"runs":[{"text":"V"}]},"lengthSeconds":"60"}}]}}]}}}}]}}}`

// TestFacade_EnumerateChannelURLResolvesUploads routes a channel handle to its
// uploads playlist (VLUU) and stamps every entry with the resolved channel ID.
func TestFacade_EnumerateChannelURLResolvesUploads(t *testing.T) {
	const channelID = "UCabcdefghijklmnopqrstuv"
	var browseID string
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/navigation/resolve_url"):
			return resp(http.StatusOK, []byte(`{"endpoint":{"browseEndpoint":{"browseId":"`+channelID+`"}}}`)), nil
		case strings.HasSuffix(r.URL.Path, "/browse"):
			b, _ := io.ReadAll(r.Body)
			if _, after, ok := bytes.Cut(b, []byte(`"browseId":"`)); ok {
				if id, _, ok := bytes.Cut(after, []byte(`"`)); ok {
					browseID = string(id)
				}
			}
			return resp(http.StatusOK, []byte(minimalBrowse)), nil
		default:
			return resp(http.StatusOK, nil), nil // homepage bootstrap, best-effort
		}
	})
	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}

	pl, err := c.Enumerate(context.Background(), "https://www.youtube.com/@SomeHandle", waxtap.EnumerateOptions{})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	// The channel resolved to its uploads playlist: browseId is VL + UU + channel tail.
	if want := "VLUU" + channelID[2:]; browseID != want {
		t.Errorf("browse browseId = %q, want %q (uploads playlist)", browseID, want)
	}
	if len(pl.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(pl.Entries))
	}
	if pl.Entries[0].ChannelID != channelID {
		t.Errorf("entry ChannelID = %q, want the resolved %q (channel-feed stamp)", pl.Entries[0].ChannelID, channelID)
	}
}

// TestFacade_EnumerateChannelURLWithListHonorsPlaylist checks an explicit list=
// on a channel URL browses that playlist, not the channel's uploads feed, and
// never calls the channel resolver.
func TestFacade_EnumerateChannelURLWithListHonorsPlaylist(t *testing.T) {
	var browseID string
	var resolveCalls int
	rt := roundTripFn(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/navigation/resolve_url"):
			resolveCalls++
			return resp(http.StatusNotFound, nil), nil
		case strings.HasSuffix(r.URL.Path, "/browse"):
			b, _ := io.ReadAll(r.Body)
			if _, after, ok := bytes.Cut(b, []byte(`"browseId":"`)); ok {
				if id, _, ok := bytes.Cut(after, []byte(`"`)); ok {
					browseID = string(id)
				}
			}
			return resp(http.StatusOK, []byte(minimalBrowse)), nil
		default:
			return resp(http.StatusOK, nil), nil
		}
	})
	c, err := waxtap.New(waxtap.Options{HTTPClient: &http.Client{Transport: rt}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Enumerate(context.Background(), "https://www.youtube.com/@SomeHandle?list=PLabcdefghij", waxtap.EnumerateOptions{})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}
	if browseID != "VLPLabcdefghij" {
		t.Errorf("browse browseId = %q, want VLPLabcdefghij (explicit list wins)", browseID)
	}
	if resolveCalls != 0 {
		t.Errorf("resolveCalls = %d, want 0 (no channel resolution when list= is present)", resolveCalls)
	}
}
