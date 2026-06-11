// Package sponsorblock defines the SponsorBlock category vocabulary used by
// WaxTap cut requests.
package sponsorblock

// Category is a SponsorBlock segment category. Values match the API's wire
// strings exactly.
type Category string

const (
	CategorySponsor       Category = "sponsor"        // paid promotion
	CategorySelfPromo     Category = "selfpromo"      // unpaid self-promotion
	CategoryInteraction   Category = "interaction"    // engagement reminder
	CategoryIntro         Category = "intro"          // intermission or intro animation
	CategoryOutro         Category = "outro"          // credits or end card
	CategoryPreview       Category = "preview"        // recap or preview
	CategoryFiller        Category = "filler"         // tangential filler
	CategoryMusicOffTopic Category = "music_offtopic" // non-music section in a music video
)

// DefaultCategories is used when a caller enables SponsorBlock without naming
// categories. WaxTap's default music use case removes non-music interludes.
var DefaultCategories = []Category{CategoryMusicOffTopic}

// Valid reports whether c is a recognized category.
func (c Category) Valid() bool {
	switch c {
	case CategorySponsor, CategorySelfPromo, CategoryInteraction,
		CategoryIntro, CategoryOutro, CategoryPreview,
		CategoryFiller, CategoryMusicOffTopic:
		return true
	default:
		return false
	}
}
