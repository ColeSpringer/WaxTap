package waxtap

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/internal/mediatest"
	"github.com/colespringer/waxtap/v3/youtube"
)

func TestTagsFor(t *testing.T) {
	uploaded := time.Date(2017, 11, 15, 0, 47, 18, 0, time.UTC)
	artTrack := func(mut func(*youtube.Music)) *youtube.Music {
		m := &youtube.Music{
			Title: "Night Drive", Artists: []string{"Small Artist", "Guest Artist"},
			Album: "Night Drive EP", Source: youtube.MusicFromDescription,
		}
		if mut != nil {
			mut(m)
		}
		return m
	}

	cases := []struct {
		name string
		v    youtube.Video
		want embedTags
	}{
		{
			// The year is all the tag takes: the day is on Video.PublishDate
			// for a caller that wants it.
			name: "an ordinary video is credited to its channel and dated by its upload year",
			v:    youtube.Video{Title: "A Video", Author: "Some Channel", PublishDate: uploaded},
			want: embedTags{title: "A Video", artists: []string{"Some Channel"}, year: "2017"},
		},
		{
			name: "no year is written when none is known",
			v:    youtube.Video{Title: "A Video", Author: "Some Channel"},
			want: embedTags{title: "A Video", artists: []string{"Some Channel"}},
		},
		{
			// The description did not read as an Art Track's, but the channel
			// is still an auto-generated one and its suffix is not a name.
			name: "a Topic channel loses its suffix",
			v:    youtube.Video{Title: "Night Drive", Author: "Small Artist - Topic"},
			want: embedTags{title: "Night Drive", artists: []string{"Small Artist"}},
		},
		{
			name: "a channel that only mentions a topic keeps its name",
			v:    youtube.Video{Title: "A Video", Author: "Off - Topic Podcast"},
			want: embedTags{title: "A Video", artists: []string{"Off - Topic Podcast"}},
		},
		{
			name: "no artist is written for a video with no channel name",
			v:    youtube.Video{Title: "A Video"},
			want: embedTags{title: "A Video"},
		},
		{
			name: "an Art Track's stated artists, album, release year and copyright",
			v: youtube.Video{
				Title: "Night Drive", Author: "Small Artist - Topic", PublishDate: uploaded,
				Music: artTrack(func(m *youtube.Music) {
					m.ReleaseDate = time.Date(1981, 10, 26, 0, 0, 0, 0, time.UTC)
					m.Copyright = "℗ 1981 Fixture Records"
				}),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				year: "1981", copyright: "℗ 1981 Fixture Records",
			},
		},
		{
			// The label's own date wins over both other years, whichever is
			// earlier: a remaster's copyright can predate its release.
			name: "a stated release date outranks the copyright and upload years",
			v: youtube.Video{
				Title: "Night Drive", PublishDate: uploaded,
				Music: artTrack(func(m *youtube.Music) {
					m.ReleaseDate = time.Date(1992, 5, 26, 0, 0, 0, 0, time.UTC)
					m.Copyright = "℗ 1981 Fixture Records"
				}),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				year: "1992", copyright: "℗ 1981 Fixture Records",
			},
		},
		{
			// A catalog track delivered years after it came out: the upload
			// year would date the recording by its delivery.
			name: "an earlier copyright year dates a track with no release date",
			v: youtube.Video{
				Title: "Night Drive", PublishDate: uploaded,
				Music: artTrack(func(m *youtube.Music) { m.Copyright = "℗ 1981, 2017 Fixture Records Ltd" }),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				year: "1981", copyright: "℗ 1981, 2017 Fixture Records Ltd",
			},
		},
		{
			name: "a new release's copyright year is its upload year",
			v: youtube.Video{
				Title: "Night Drive", PublishDate: uploaded,
				Music: artTrack(func(m *youtube.Music) { m.Copyright = "℗ 2017 Fixture Records" }),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				year: "2017", copyright: "℗ 2017 Fixture Records",
			},
		},
		{
			name: "a copyright year after the upload is not a release year",
			v: youtube.Video{
				Title: "Night Drive", PublishDate: uploaded,
				Music: artTrack(func(m *youtube.Music) { m.Copyright = "℗ 2101 Records" }),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				year: "2017", copyright: "℗ 2101 Records",
			},
		},
		{
			name: "a copyright line with no year leaves the upload year",
			v: youtube.Video{
				Title: "Night Drive", PublishDate: uploaded,
				Music: artTrack(func(m *youtube.Music) { m.Copyright = "℗ Small Artist" }),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				year: "2017", copyright: "℗ Small Artist",
			},
		},
		{
			name: "a label's number is not a year",
			v: youtube.Video{
				Title: "Night Drive", PublishDate: uploaded,
				Music: artTrack(func(m *youtube.Music) { m.Copyright = "℗ 1017 Fixture Records" }),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				year: "2017", copyright: "℗ 1017 Fixture Records",
			},
		},
		{
			name: "a longer number is not a year",
			v: youtube.Video{
				Title: "Night Drive", PublishDate: uploaded,
				Music: artTrack(func(m *youtube.Music) { m.Copyright = "℗ 19810 Fixture Records" }),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				year: "2017", copyright: "℗ 19810 Fixture Records",
			},
		},
		{
			// The default client reports no upload date, so the year is all
			// there is.
			name: "the copyright year stands in when the upload date is unknown",
			v: youtube.Video{
				Title: "Night Drive",
				Music: artTrack(func(m *youtube.Music) { m.Copyright = "℗1981 Fixture Records" }),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				year: "1981", copyright: "℗1981 Fixture Records",
			},
		},
		{
			name: "a future year is not a release year even with nothing to hold it against",
			v: youtube.Video{
				Title: "Night Drive",
				Music: artTrack(func(m *youtube.Music) { m.Copyright = "℗ 2101 Records" }),
			},
			want: embedTags{
				title: "Night Drive", artists: []string{"Small Artist", "Guest Artist"}, album: "Night Drive EP",
				copyright: "℗ 2101 Records",
			},
		},
		{
			// YouTube cuts a long Art Track title; the description states it
			// whole, and that is the track's title.
			name: "an Art Track's title is the description's when the video's was cut short",
			v: youtube.Video{
				Title: "A Long Title: IV. Presto - Allegro...",
				Music: artTrack(func(m *youtube.Music) { m.Title = "A Long Title: IV. Presto - Allegro assai (Live)" }),
			},
			want: embedTags{
				title: "A Long Title: IV. Presto - Allegro assai (Live)", artists: []string{"Small Artist", "Guest Artist"},
				album: "Night Drive EP",
			},
		},
		{
			// A New Year's Eve upload: YouTube states it in its own zone, and
			// the year is that date's, not the UTC instant's.
			name: "the upload year is the year of the date YouTube states",
			v: youtube.Video{
				Title: "A Video", Author: "Some Channel",
				PublishDate: time.Date(2016, 12, 31, 18, 0, 0, 0, time.FixedZone("", -8*60*60)),
			},
			want: embedTags{title: "A Video", artists: []string{"Some Channel"}, year: "2016"},
		},
		{
			// The card names the song the video carries; the video keeps the
			// title it was given, which can say more (a featured credit, a
			// version).
			name: "the Music card fills artist and album and leaves the title",
			v: youtube.Video{
				Title: "SMALL ARTIST - Night Drive ft. Guest Artist", Author: "SmallArtistVEVO", PublishDate: uploaded,
				Music: &youtube.Music{
					Title: "Night Drive", Artists: []string{"Small Artist"}, Album: "Night Drive EP",
					Source: youtube.MusicFromWatchPage,
				},
			},
			want: embedTags{
				title: "SMALL ARTIST - Night Drive ft. Guest Artist", artists: []string{"Small Artist"},
				album: "Night Drive EP", year: "2017",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tagsFor(&c.v); !reflect.DeepEqual(got, c.want) {
				t.Errorf("tagsFor = %+v, want %+v", got, c.want)
			}
		})
	}
}

// encodeFixture renders a short tone into dir in the container ext names.
func encodeFixture(t *testing.T, c *Client, dir, ext string, codec media.Codec) string {
	t.Helper()
	wav := filepath.Join(dir, "tone.wav")
	if err := os.WriteFile(wav, mediatest.SineWAV(1, 2), 0o644); err != nil {
		t.Fatal(err)
	}
	if ext == "wav" {
		return wav
	}
	out := filepath.Join(dir, "tone."+ext)
	if _, err := c.engine().Transcode(context.Background(), wav, out, media.Spec{Codec: codec}); err != nil {
		t.Fatalf("encode %s fixture: %v", ext, err)
	}
	return out
}

// An Art Track's tags land in every container a download can be delivered in,
// read back as written and without a warning: a multi-artist credit must not
// cost every Art Track download a metadata-embed-failed report.
func TestDoEmbedWritesMusicTagsIntoEveryContainer(t *testing.T) {
	ctx := context.Background()
	c := newOfflineClient(t)
	v := &youtube.Video{
		ID: "dummyVideo0", Title: "Night Drive", Author: "Small Artist - Topic",
		PublishDate: time.Date(2019, 3, 12, 10, 16, 47, 0, time.UTC),
		Music: &youtube.Music{
			Title: "Night Drive", Artists: []string{"Small Artist", "Guest Artist"}, Album: "Night Drive EP",
			ReleaseDate: time.Date(2019, 3, 13, 0, 0, 0, 0, time.UTC),
			Copyright:   "℗ 2019 Fixture Records",
			Source:      youtube.MusicFromDescription,
		},
	}

	for _, f := range []struct {
		ext   string
		codec media.Codec
	}{
		{"wav", media.CodecWAV},
		{"flac", media.CodecFLAC},
		{"mp3", media.CodecMP3},
		{"m4a", media.CodecAAC},
		{"opus", media.CodecOpus},
		{"webm", media.CodecOpus},
		{"mka", media.CodecOpus},
		{"ogg", media.CodecVorbis},
		{"aiff", media.CodecAIFF},
		{"wv", media.CodecWavPack},
		{"ape", media.CodecAPE},
	} {
		t.Run(f.ext, func(t *testing.T) {
			path := encodeFixture(t, c, t.TempDir(), f.ext, f.codec)
			_, skip, err := c.doEmbed(ctx, path, f.ext, v, embedOptions{metadata: true})
			if err != nil {
				t.Fatalf("doEmbed: %v", err)
			}
			if skip != "" {
				t.Errorf("skipReason = %q, want none", skip)
			}

			doc, err := waxlabel.ParseFile(ctx, path)
			if err != nil {
				t.Fatalf("parse tagged file: %v", err)
			}
			for key, want := range map[tag.Key][]string{
				tag.Title:         {"Night Drive"},
				tag.Album:         {"Night Drive EP"},
				tag.RecordingDate: {"2019"},
				tag.Copyright:     {"℗ 2019 Fixture Records"},
			} {
				if got, _ := doc.Get(key); !slices.Equal(got, want) {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			// A credit list, not a joined name a reader would have to split.
			if got, _ := doc.Get(tag.Artist); !slices.Equal(got, []string{"Small Artist", "Guest Artist"}) {
				t.Errorf("ARTIST = %q, want the two artists as two values", got)
			}
		})
	}
}

// The watch-page pass backfills a Music the extraction did not find and never
// replaces one it did: the description states more than the card.
func TestMergeWatchPageMetaMusic(t *testing.T) {
	card := &youtube.Music{Title: "Night Drive", Artists: []string{"Small Artist"}, Source: youtube.MusicFromWatchPage}

	t.Run("fills a video that stated none", func(t *testing.T) {
		v := &youtube.Video{}
		mergeWatchPageMeta(v, youtube.WatchPageMeta{Music: card})
		if v.Music != card {
			t.Errorf("Music = %+v, want the card's", v.Music)
		}
	})
	t.Run("keeps the description's", func(t *testing.T) {
		stated := &youtube.Music{Title: "Night Drive", Artists: []string{"Small Artist", "Guest Artist"}, Source: youtube.MusicFromDescription}
		v := &youtube.Video{Music: stated}
		mergeWatchPageMeta(v, youtube.WatchPageMeta{Music: card})
		if v.Music != stated {
			t.Errorf("Music = %+v, want the description's kept", v.Music)
		}
	})
}

func TestVideoMetadataForCarriesMusic(t *testing.T) {
	m := &youtube.Music{Title: "Night Drive", Artists: []string{"Small Artist"}, Source: youtube.MusicFromDescription}
	md := videoMetadataFor(Request{ProcessSpec: ProcessSpec{IncludeMetadata: true}}, &youtube.Video{Music: m})
	if md == nil || md.Music != m {
		t.Fatalf("Metadata = %+v, want it to carry the video's Music", md)
	}
}
