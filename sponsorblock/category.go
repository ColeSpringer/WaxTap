// Package sponsorblock defines the SponsorBlock category vocabulary used by
// WaxTap cut requests.
package sponsorblock

// Category is a SponsorBlock segment category. Values match the API's wire
// strings exactly.
type Category string

const (
	CategorySponsor       Category = "sponsor"
	CategorySelfPromo     Category = "selfpromo"
	CategoryInteraction   Category = "interaction"
	CategoryIntro         Category = "intro"
	CategoryOutro         Category = "outro"
	CategoryPreview       Category = "preview"
	CategoryFiller        Category = "filler"
	CategoryMusicOffTopic Category = "music_offtopic"
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
