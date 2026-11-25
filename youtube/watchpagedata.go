package youtube

import (
	"encoding/json"
	"errors"
	"time"
)

// ytInitialData is the subset of the watch page's ytInitialData that WaxTap
// reads: the chapter markers, nested under the decorated player bar (the
// renderer wraps a same-named renderer), and the engagement panels, one of
// which is the description. The heatmap lives in a sibling markersMap entry
// whose value has no chapters array, so it is skipped by selection.
type ytInitialData struct {
	PlayerOverlays struct {
		PlayerOverlayRenderer struct {
			DecoratedPlayerBarRenderer struct {
				DecoratedPlayerBarRenderer struct {
					PlayerBar struct {
						MultiMarkersPlayerBarRenderer struct {
							MarkersMap []chapterMarkerEntry `json:"markersMap"`
						} `json:"multiMarkersPlayerBarRenderer"`
					} `json:"playerBar"`
				} `json:"decoratedPlayerBarRenderer"`
			} `json:"decoratedPlayerBarRenderer"`
		} `json:"playerOverlayRenderer"`
	} `json:"playerOverlays"`

	// EngagementPanels is held undecoded and read on its own (see
	// descriptionPanel), so a shape this reader does not expect costs the
	// Music card at most and never the chapters decoded beside it.
	EngagementPanels json.RawMessage `json:"engagementPanels"`
}

// engagementPanel is one of the page's side panels. Only the description's
// carries a structured description.
type engagementPanel struct {
	EngagementPanelSectionListRenderer struct {
		Content struct {
			StructuredDescriptionContentRenderer *structuredDescription `json:"structuredDescriptionContentRenderer"`
		} `json:"content"`
	} `json:"engagementPanelSectionListRenderer"`
}

// structuredDescription is the description panel: the description itself
// followed by what YouTube attaches to it. It lays that out in one of two ways
// on any given response, and the music is in both: as a shelf of cards, or as
// a row of a details list with the album on a card of its own (see
// musicCard).
type structuredDescription struct {
	Items []struct {
		HorizontalCardListRenderer *cardShelf          `json:"horizontalCardListRenderer"`
		ItemSectionRenderer        *descriptionSection `json:"itemSectionRenderer"`
	} `json:"items"`
}

// descriptionPanel returns the description among the page's engagement
// panels, or nil when raw holds none.
//
// A value of a type this reader does not expect is skipped and the rest is
// still decoded, which is what the shelves need: they are YouTube's to
// reshape one at a time, and one reshaped shelf must not hide the Music shelf
// beside it.
func descriptionPanel(raw json.RawMessage) *structuredDescription {
	var panels []engagementPanel
	if err := json.Unmarshal(raw, &panels); err != nil {
		if _, mistyped := errors.AsType[*json.UnmarshalTypeError](err); !mistyped {
			return nil
		}
	}
	for _, p := range panels {
		if sd := p.EngagementPanelSectionListRenderer.Content.StructuredDescriptionContentRenderer; sd != nil {
			return sd
		}
	}
	return nil
}

// initialDataMarkers precede the ytInitialData object in the two shapes a
// watch page states it in, and which one a response uses is YouTube's choice:
// assigned to the variable of that name, or as the text of a JSON script
// element that a bootstrap line parses into it.
var initialDataMarkers = []string{"ytInitialData", `id="yt-initial-data"`}

// parseWatchPageData reads what the watch page's ytInitialData adds to a
// player response: the chapters, bounded by duration as buildChapters
// describes, and the Music card, with hasCard false when the page shows no
// card for exactly one song. Neither is in the /player response, so both are
// reachable only when the watch page has been fetched.
//
// It scans successive matches of each marker, since a match can be a decoy
// assignment or a mention followed by script rather than data, and takes each
// answer from the first object that carries its part of the page: the player
// bar for the chapters (possibly none, e.g. a heatmap-only bar), the
// description panel for the card. One object holds both, so a real page is
// decoded once.
func parseWatchPageData(body []byte, duration time.Duration) (chapters []Chapter, card musicCard, hasCard bool) {
	html := string(body)
	var items []chapterItem
	foundBar, foundDescription := false, false
	for _, marker := range initialDataMarkers {
		for start := 0; !foundBar || !foundDescription; {
			obj, end, ok := extractJSONObjectFrom(html, marker, start)
			if !ok {
				break
			}
			start = end
			var data ytInitialData
			if err := json.Unmarshal(obj, &data); err != nil {
				continue // a decoy or unrelated object; try the next match
			}
			markersMap := data.PlayerOverlays.PlayerOverlayRenderer.
				DecoratedPlayerBarRenderer.DecoratedPlayerBarRenderer.
				PlayerBar.MultiMarkersPlayerBarRenderer.MarkersMap
			if !foundBar && len(markersMap) > 0 {
				items, foundBar = selectChapterItems(markersMap), true
			}
			if foundDescription {
				continue
			}
			if sd := descriptionPanel(data.EngagementPanels); sd != nil {
				card, hasCard = sd.musicCard()
				foundDescription = true
			}
		}
	}
	return buildChapters(items, duration), card, hasCard
}
