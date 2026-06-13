package youtube

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strconv"
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
									Contents []playlistItem `json:"contents"`
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

// playlistItem is one entry of a playlist item list, in any of the shapes
// YouTube serves: a legacy video, a 2025 lockup view-model video, a
// continuation marker (legacy or view-model form), the legacy single-wrapper
// list (only on initial pages), or a "no videos" notice.
type playlistItem struct {
	PlaylistVideoRenderer *struct {
		VideoID       string   `json:"videoId"`
		Title         textRuns `json:"title"`
		ShortByline   textRuns `json:"shortBylineText"`
		LengthSeconds string   `json:"lengthSeconds"`
	} `json:"playlistVideoRenderer"`
	LockupViewModel           *lockupViewModel    `json:"lockupViewModel"`
	ContinuationItemRenderer  *continuationMarker `json:"continuationItemRenderer"`
	ContinuationItemViewModel *continuationMarker `json:"continuationItemViewModel"`
	// PlaylistVideoListRenderer is the legacy wrapper that nests the items one
	// level deeper; it appears only among an initial page's section contents.
	PlaylistVideoListRenderer *playlistVideoList `json:"playlistVideoListRenderer"`
	// MessageRenderer is YouTube's "no videos in this playlist" notice. Its
	// presence confirms an empty playlist rather than a parse failure.
	MessageRenderer *struct {
		Text textRuns `json:"text"`
	} `json:"messageRenderer"`
}

// isItem reports whether the entry matches a known video or continuation
// shape, meaning the section entries are themselves the playlist items (the
// lockup layout) rather than a legacy wrapper.
func (it playlistItem) isItem() bool {
	return it.PlaylistVideoRenderer != nil || it.LockupViewModel != nil ||
		it.ContinuationItemRenderer != nil || it.ContinuationItemViewModel != nil
}

// continuationMarker is the continuation entry in either naming
// (continuationItemRenderer or continuationItemViewModel). The token has been
// observed in three homes, all covered by token(): directly under
// continuationEndpoint.continuationCommand, nested in the endpoint's
// commandExecutorCommand list, or (view-model pages) under
// continuationCommand.innertubeCommand.
type continuationMarker struct {
	ContinuationEndpoint continuationEndpoint `json:"continuationEndpoint"`
	ContinuationCommand  struct {
		InnertubeCommand continuationEndpoint `json:"innertubeCommand"`
	} `json:"continuationCommand"`
}

func (m continuationMarker) token() string {
	if t := m.ContinuationEndpoint.token(); t != "" {
		return t
	}
	return m.ContinuationCommand.InnertubeCommand.token()
}

// continuationEndpoint carries a continuation token either directly or inside
// a commandExecutorCommand list (live pages bundle the token with unrelated
// commands such as a voting-refresh popup).
type continuationEndpoint struct {
	ContinuationCommand struct {
		Token string `json:"token"`
	} `json:"continuationCommand"`
	CommandExecutorCommand struct {
		Commands []struct {
			ContinuationCommand struct {
				Token string `json:"token"`
			} `json:"continuationCommand"`
		} `json:"commands"`
	} `json:"commandExecutorCommand"`
}

func (e continuationEndpoint) token() string {
	if e.ContinuationCommand.Token != "" {
		return e.ContinuationCommand.Token
	}
	for _, c := range e.CommandExecutorCommand.Commands {
		if c.ContinuationCommand.Token != "" {
			return c.ContinuationCommand.Token
		}
	}
	return ""
}

func (it playlistItem) toEntry(index int) (PlaylistEntry, error) {
	if r := it.PlaylistVideoRenderer; r != nil && r.VideoID != "" {
		return PlaylistEntry{
			VideoID:  r.VideoID,
			Title:    r.Title.String(),
			Author:   r.ShortByline.String(),
			Duration: time.Duration(atoi(r.LengthSeconds)) * time.Second,
			Index:    index,
		}, nil
	}
	if l := it.LockupViewModel; l != nil && l.ContentID != "" {
		// Only video lockups become entries: playlist, mix, and podcast lockups
		// share this view model and their contentId is not a video ID. An absent
		// contentType is accepted so older pages keep parsing.
		if l.ContentType != "" && l.ContentType != "LOCKUP_CONTENT_TYPE_VIDEO" {
			return PlaylistEntry{}, fmt.Errorf("playlist item %d (lockup %s) is %s, not a video", index, l.ContentID, l.ContentType)
		}
		title := l.Metadata.LockupMetadataViewModel.Title.Content
		if title == "" {
			return PlaylistEntry{}, fmt.Errorf("playlist item %d (lockup %s) has no title", index, l.ContentID)
		}
		// Author and duration are best-effort: the lockup metadata rows carry
		// the channel, and the thumbnail badge carries a clock string. Missing
		// values are left zero for the opt-in Enrich pass to fill.
		return PlaylistEntry{
			VideoID:  l.ContentID,
			Title:    title,
			Author:   l.author(),
			Duration: l.duration(),
			Index:    index,
		}, nil
	}
	return PlaylistEntry{}, fmt.Errorf("playlist item %d has no video", index)
}

// lockupViewModel is the view-model item shape YouTube A/B-serves in place of
// playlistVideoRenderer since 2025. Text nodes here are {content} strings, not
// the runs/simpleText form.
type lockupViewModel struct {
	ContentID   string `json:"contentId"`
	ContentType string `json:"contentType"` // e.g. LOCKUP_CONTENT_TYPE_VIDEO
	Metadata    struct {
		LockupMetadataViewModel struct {
			Title struct {
				Content string `json:"content"`
			} `json:"title"`
			Metadata struct {
				ContentMetadataViewModel struct {
					MetadataRows []struct {
						MetadataParts []struct {
							Text struct {
								Content string `json:"content"`
							} `json:"text"`
						} `json:"metadataParts"`
					} `json:"metadataRows"`
				} `json:"contentMetadataViewModel"`
			} `json:"metadata"`
		} `json:"lockupMetadataViewModel"`
	} `json:"metadata"`
	ContentImage struct {
		ThumbnailViewModel struct {
			Overlays []struct {
				ThumbnailOverlayBadgeViewModel struct {
					ThumbnailBadges []thumbnailBadge `json:"thumbnailBadges"`
				} `json:"thumbnailOverlayBadgeViewModel"`
				ThumbnailBottomOverlayViewModel struct {
					Badges []thumbnailBadge `json:"badges"`
				} `json:"thumbnailBottomOverlayViewModel"`
			} `json:"overlays"`
		} `json:"thumbnailViewModel"`
	} `json:"contentImage"`
}

// thumbnailBadge is one badge of a thumbnail overlay; the duration badge's
// Text is a clock string. Both observed overlay forms
// (thumbnailOverlayBadgeViewModel and thumbnailBottomOverlayViewModel) nest
// the same badge type.
type thumbnailBadge struct {
	ThumbnailBadgeViewModel struct {
		Text string `json:"text"`
	} `json:"thumbnailBadgeViewModel"`
}

// author returns the first metadata-row text, which for playlist videos is the
// channel name. Best-effort: an absent row yields "".
func (l *lockupViewModel) author() string {
	for _, row := range l.Metadata.LockupMetadataViewModel.Metadata.ContentMetadataViewModel.MetadataRows {
		for _, part := range row.MetadataParts {
			if part.Text.Content != "" {
				return part.Text.Content
			}
		}
	}
	return ""
}

// duration returns the clock duration from the thumbnail overlay badge, the
// only place a lockup carries one. Best-effort: no parseable badge yields 0.
func (l *lockupViewModel) duration() time.Duration {
	for _, ov := range l.ContentImage.ThumbnailViewModel.Overlays {
		for _, badge := range ov.ThumbnailOverlayBadgeViewModel.ThumbnailBadges {
			if d, ok := parseBadgeDuration(badge.ThumbnailBadgeViewModel.Text); ok {
				return d
			}
		}
		for _, badge := range ov.ThumbnailBottomOverlayViewModel.Badges {
			if d, ok := parseBadgeDuration(badge.ThumbnailBadgeViewModel.Text); ok {
				return d
			}
		}
	}
	return 0
}

// parseBadgeDuration parses a thumbnail badge clock string ("3:05" or
// "1:02:03"). Non-clock badges such as "LIVE" report false.
func parseBadgeDuration(s string) (time.Duration, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + n
	}
	return time.Duration(total) * time.Second, true
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
			// Preserve YouTube's reason while distinguishing an unavailable playlist
			// from a malformed ID.
			return playlistMeta{}, nil, "", &waxerr.PlaylistUnavailableError{Reason: a.AlertRenderer.Text.String()}
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

	isr := firstItemSection(br)
	list := playlistListFrom(isr)
	if len(list.Contents) == 0 {
		return meta, nil, "", zeroItemsError(isr)
	}
	entries, token := splitItems(list.Contents)
	if token == "" {
		token = list.legacyToken()
	}
	return meta, entries, token, nil
}

// zeroItemsError classifies an alert-free page with no parsed items. An empty
// legacy wrapper or a "no videos" notice confirms an empty playlist. Any other
// shape indicates that the parser may be stale.
func zeroItemsError(isr []playlistItem) error {
	for _, it := range isr {
		if it.PlaylistVideoListRenderer != nil || it.MessageRenderer != nil {
			return waxerr.ErrPlaylistEmpty
		}
	}
	return fmt.Errorf("no recognized playlist contents: %w", waxerr.ErrPlaylistParse)
}

// parseBrowseContinuation parses a continuation page, handling both the modern
// onResponseReceivedActions shape and the legacy continuationContents shape. A
// page that parses to neither entries nor a further token is classified as a
// stale parser, the same signal parseBrowseInitial gives for page one:
// returning success would silently truncate the enumeration when YouTube
// renames the action or marker shapes. (A marker rename on a page that still
// yields entries is indistinguishable from the legitimate final page and
// cannot be detected here.)
func parseBrowseContinuation(body []byte) ([]playlistItem, string, error) {
	var cr continuationResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, "", fmt.Errorf("decode continuation: %w", err)
	}
	var entries []playlistItem
	var token string
	if len(cr.OnResponseReceivedActions) > 0 {
		entries, token = splitItems(cr.OnResponseReceivedActions[0].AppendContinuationItemsAction.ContinuationItems)
	} else {
		// Legacy shape.
		list := cr.ContinuationContents.PlaylistVideoListContinuation
		entries, token = splitItems(list.Contents)
		if token == "" {
			token = list.legacyToken()
		}
	}
	if len(entries) == 0 && token == "" {
		return nil, "", fmt.Errorf("no recognized continuation contents: %w", waxerr.ErrPlaylistParse)
	}
	return entries, token, nil
}

// firstItemSection returns the contents of the first item section, the level
// at which both playlist layouts carry their items.
func firstItemSection(br browseResponse) []playlistItem {
	tabs := br.Contents.TwoColumnBrowseResultsRenderer.Tabs
	if len(tabs) == 0 {
		return nil
	}
	sections := tabs[0].TabRenderer.Content.SectionListRenderer.Contents
	if len(sections) == 0 {
		return nil
	}
	return sections[0].ItemSectionRenderer.Contents
}

// playlistListFrom extracts the item list from a section's contents. The
// legacy layout nests the items one level deeper in a single
// playlistVideoListRenderer wrapper; the lockup layout serves the items
// directly as section entries.
func playlistListFrom(isr []playlistItem) playlistVideoList {
	for _, it := range isr {
		if it.PlaylistVideoListRenderer != nil {
			return *it.PlaylistVideoListRenderer
		}
	}
	for _, it := range isr {
		if it.isItem() {
			return playlistVideoList{Contents: isr}
		}
	}
	return playlistVideoList{}
}

// splitItems separates video entries from the continuation marker, which may
// arrive in the legacy renderer or the view-model form (lockup pages have been
// seen with either).
func splitItems(items []playlistItem) (entries []playlistItem, token string) {
	for _, it := range items {
		if m := cmp.Or(it.ContinuationItemRenderer, it.ContinuationItemViewModel); m != nil {
			if t := m.token(); t != "" {
				token = t
			}
			continue
		}
		entries = append(entries, it)
	}
	return entries, token
}
