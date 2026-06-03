package youtube

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

// playlistMeta is the playlist-level metadata pulled from a browse response.
type playlistMeta struct {
	title       string
	author      string
	visitorData string
}

// browseResponse is the subset of the InnerTube /browse response used to
// enumerate a playlist (web client shape).
type browseResponse struct {
	ResponseContext struct {
		VisitorData string `json:"visitorData"`
	} `json:"responseContext"`
	Metadata struct {
		PlaylistMetadataRenderer struct {
			Title string `json:"title"`
		} `json:"playlistMetadataRenderer"`
	} `json:"metadata"`
	Header struct {
		PlaylistHeaderRenderer struct {
			Title     textRuns `json:"title"`
			OwnerText textRuns `json:"ownerText"`
		} `json:"playlistHeaderRenderer"`
	} `json:"header"`
	Sidebar struct {
		PlaylistSidebarRenderer struct {
			Items []struct {
				PlaylistSidebarSecondaryInfoRenderer struct {
					VideoOwner struct {
						VideoOwnerRenderer struct {
							Title textRuns `json:"title"`
						} `json:"videoOwnerRenderer"`
					} `json:"videoOwner"`
				} `json:"playlistSidebarSecondaryInfoRenderer"`
			} `json:"items"`
		} `json:"playlistSidebarRenderer"`
	} `json:"sidebar"`
	Contents struct {
		TwoColumnBrowseResultsRenderer struct {
			Tabs []struct {
				TabRenderer struct {
					Content struct {
						SectionListRenderer struct {
							Contents []struct {
								ItemSectionRenderer struct {
									Contents []struct {
										PlaylistVideoListRenderer playlistVideoList `json:"playlistVideoListRenderer"`
									} `json:"contents"`
								} `json:"itemSectionRenderer"`
							} `json:"contents"`
						} `json:"sectionListRenderer"`
					} `json:"content"`
				} `json:"tabRenderer"`
			} `json:"tabs"`
		} `json:"twoColumnBrowseResultsRenderer"`
	} `json:"contents"`
	Alerts []struct {
		AlertRenderer struct {
			Type string   `json:"type"`
			Text textRuns `json:"text"`
		} `json:"alertRenderer"`
	} `json:"alerts"`
}

// continuationResponse is a paginated browse continuation. YouTube uses two
// shapes: the modern onResponseReceivedActions, and a legacy continuationContents
// form (still returned for some playlists/clients). Both are handled.
type continuationResponse struct {
	OnResponseReceivedActions []struct {
		AppendContinuationItemsAction struct {
			ContinuationItems []playlistItem `json:"continuationItems"`
		} `json:"appendContinuationItemsAction"`
	} `json:"onResponseReceivedActions"`
	ContinuationContents struct {
		PlaylistVideoListContinuation playlistVideoList `json:"playlistVideoListContinuation"`
	} `json:"continuationContents"`
}

// playlistVideoList is the contents+continuations container shared by the
// initial page (playlistVideoListRenderer) and the legacy continuation shape
// (playlistVideoListContinuation).
type playlistVideoList struct {
	Contents      []playlistItem `json:"contents"`
	Continuations []struct {
		NextContinuationData struct {
			Continuation string `json:"continuation"`
		} `json:"nextContinuationData"`
	} `json:"continuations"`
}

// legacyToken returns the pre-2020 continuation token, used as a fallback when
// no modern continuationItemRenderer token is present in the contents.
func (l playlistVideoList) legacyToken() string {
	if len(l.Continuations) > 0 {
		return l.Continuations[0].NextContinuationData.Continuation
	}
	return ""
}

// playlistItem is either a video entry or a continuation marker.
type playlistItem struct {
	PlaylistVideoRenderer *struct {
		VideoID       string   `json:"videoId"`
		Title         textRuns `json:"title"`
		ShortByline   textRuns `json:"shortBylineText"`
		LengthSeconds string   `json:"lengthSeconds"`
	} `json:"playlistVideoRenderer"`
	ContinuationItemRenderer *struct {
		ContinuationEndpoint struct {
			ContinuationCommand struct {
				Token string `json:"token"`
			} `json:"continuationCommand"`
		} `json:"continuationEndpoint"`
	} `json:"continuationItemRenderer"`
}

func (it playlistItem) toEntry(index int) (PlaylistEntry, error) {
	r := it.PlaylistVideoRenderer
	if r == nil || r.VideoID == "" {
		return PlaylistEntry{}, fmt.Errorf("playlist item %d has no video", index)
	}
	return PlaylistEntry{
		VideoID:  r.VideoID,
		Title:    r.Title.String(),
		Author:   r.ShortByline.String(),
		Duration: time.Duration(atoi(r.LengthSeconds)) * time.Second,
		Index:    index,
	}, nil
}

// textRuns models YouTube's {runs:[{text}]} or {simpleText} text nodes.
type textRuns struct {
	SimpleText string `json:"simpleText"`
	Runs       []struct {
		Text string `json:"text"`
	} `json:"runs"`
}

func (t textRuns) String() string {
	if t.SimpleText != "" {
		return t.SimpleText
	}
	var b strings.Builder
	for _, r := range t.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

// parseBrowseInitial parses the first browse page: playlist metadata, the first
// batch of video items, and the continuation token (if any).
func parseBrowseInitial(body []byte) (playlistMeta, []playlistItem, string, error) {
	var br browseResponse
	if err := json.Unmarshal(body, &br); err != nil {
		return playlistMeta{}, nil, "", fmt.Errorf("decode browse response: %w", err)
	}

	for _, a := range br.Alerts {
		if strings.EqualFold(a.AlertRenderer.Type, "ERROR") {
			return playlistMeta{}, nil, "", fmt.Errorf("playlist unavailable: %s: %w", a.AlertRenderer.Text.String(), waxerr.ErrInvalidPlaylistID)
		}
	}

	meta := playlistMeta{
		visitorData: br.ResponseContext.VisitorData,
		title:       br.Metadata.PlaylistMetadataRenderer.Title,
		author:      br.Header.PlaylistHeaderRenderer.OwnerText.String(),
	}
	if meta.title == "" {
		meta.title = br.Header.PlaylistHeaderRenderer.Title.String()
	}
	if meta.author == "" {
		for _, item := range br.Sidebar.PlaylistSidebarRenderer.Items {
			if a := item.PlaylistSidebarSecondaryInfoRenderer.VideoOwner.VideoOwnerRenderer.Title.String(); a != "" {
				meta.author = a
				break
			}
		}
	}

	list := firstPlaylistList(br)
	if len(list.Contents) == 0 {
		return meta, nil, "", fmt.Errorf("no playlist contents (id may be invalid or empty): %w", waxerr.ErrInvalidPlaylistID)
	}
	entries, token := splitItems(list.Contents)
	if token == "" {
		token = list.legacyToken()
	}
	return meta, entries, token, nil
}

// parseBrowseContinuation parses a continuation page, handling both the modern
// onResponseReceivedActions shape and the legacy continuationContents shape.
func parseBrowseContinuation(body []byte) ([]playlistItem, string, error) {
	var cr continuationResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, "", fmt.Errorf("decode continuation: %w", err)
	}
	if len(cr.OnResponseReceivedActions) > 0 {
		entries, token := splitItems(cr.OnResponseReceivedActions[0].AppendContinuationItemsAction.ContinuationItems)
		return entries, token, nil
	}
	// Legacy shape.
	list := cr.ContinuationContents.PlaylistVideoListContinuation
	entries, token := splitItems(list.Contents)
	if token == "" {
		token = list.legacyToken()
	}
	return entries, token, nil
}

func firstPlaylistList(br browseResponse) playlistVideoList {
	tabs := br.Contents.TwoColumnBrowseResultsRenderer.Tabs
	if len(tabs) == 0 {
		return playlistVideoList{}
	}
	sections := tabs[0].TabRenderer.Content.SectionListRenderer.Contents
	if len(sections) == 0 {
		return playlistVideoList{}
	}
	isr := sections[0].ItemSectionRenderer.Contents
	if len(isr) == 0 {
		return playlistVideoList{}
	}
	return isr[0].PlaylistVideoListRenderer
}

// splitItems separates video entries from the continuation marker.
func splitItems(items []playlistItem) (entries []playlistItem, token string) {
	for _, it := range items {
		if it.ContinuationItemRenderer != nil {
			if t := it.ContinuationItemRenderer.ContinuationEndpoint.ContinuationCommand.Token; t != "" {
				token = t
			}
			continue
		}
		entries = append(entries, it)
	}
	return entries, token
}
