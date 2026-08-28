package waxtap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
	wlerr "github.com/colespringer/waxlabel/waxerr"

	"github.com/colespringer/waxtap/v3/internal/media"
	"github.com/colespringer/waxtap/v3/youtube"
)

// This file feeds metadata to the outputs whose muxer is their only tag path
// (media.Codec.MuxEmbedsTags: WavPack and APE, whose APEv2 block is written
// with the audio). WaxLabel cannot identify either format, so the carryTags /
// embedMetadata post-passes fail on their finished files rather than adding
// anything; the tags must ride the encode instead, as
// waxflow.TranscodeOptions.Tags. Only text tags have that form: pictures,
// chapters, and synced lyrics do not survive, and the callers here warn when
// something of the kind was present to lose.

// muxEmbedLikely reports whether a process spec can end up encoding to a
// mux-tagged target, so the caller should read the source's tags up front. An
// explicit WavPack/APE format always does; a copy or unset format can be
// promoted into one by the pipeline when the output extension names .wv or
// .ape (a container the source codec cannot enter, or a downmix). A false
// positive costs one wasted metadata read: the pipeline clears tags for every
// other target.
func muxEmbedLikely(s ProcessSpec) bool {
	if transcodeCodec(specFormat(s.Transcode)).MuxEmbedsTags() {
		return true
	}
	if s.Output.kind != outputFile {
		return false
	}
	return media.MuxEmbedsExt(strings.ToLower(strings.TrimPrefix(filepath.Ext(s.Output.path), ".")))
}

// sourceMuxTags reads input's metadata for mux-time embedding: WaxLabel's
// parse when the source is a format it reads, else the engine probe's
// demuxer-read tags (which is how a WavPack, APE, or WMA source keeps its own
// text tags through a re-encode). Tags describing the source's own audio
// (ReplayGain and kin) are dropped: every caller is about to re-derive the
// samples. lost lists metadata kinds that were present but have no APEv2 text
// form; it is nil when nothing is lost, and both results are nil when the
// source carries nothing readable.
func (c *Client) sourceMuxTags(ctx context.Context, input string) (tags []media.Tag, lost []string) {
	doc, err := waxlabel.ParseFile(ctx, input)
	if err != nil {
		// The demuxer fallback is for formats WaxLabel cannot identify (WavPack,
		// APE, WMA), not for readable formats whose parse failed: those carried
		// nothing before either, and taking the fallback there would also
		// silence the carry-loss report for exactly the files whose metadata is
		// in trouble.
		if !errors.Is(err, wlerr.ErrUnsupportedFormat) {
			c.log.Debug("mux tags: source not readable", "path", input, "err", err)
			return nil, nil
		}
		// Pictures and chapters are unknown on this path, so nothing is
		// reported lost; a failed probe carried nothing before either.
		pr, perr := c.engine().Probe(ctx, input)
		if perr != nil {
			return nil, nil
		}
		return media.DropOwnAudioTags(media.TagsFromMap(pr.Tags)), nil
	}
	for k, vs := range doc.Tags().All() {
		if k.DescribesOwnAudio() {
			continue
		}
		for _, v := range vs {
			tags = append(tags, media.Tag{Key: string(k), Value: v})
		}
	}
	if n := len(doc.Pictures()); n == 1 {
		lost = append(lost, "1 picture")
	} else if n > 1 {
		lost = append(lost, fmt.Sprintf("%d pictures", n))
	}
	if len(doc.Chapters()) > 0 {
		lost = append(lost, "chapters")
	}
	if len(doc.SyncedLyrics()) > 0 {
		lost = append(lost, "synced lyrics")
	}
	return tags, lost
}

// videoMuxTags maps a YouTube video's metadata onto mux-time tags, the same
// fields and spellings the WaxLabel embed pass writes (doEmbed): TITLE,
// ARTIST from the channel name, and the publish date when present.
func videoMuxTags(v *youtube.Video) []media.Tag {
	if v == nil {
		return nil
	}
	var tags []media.Tag
	if v.Title != "" {
		tags = append(tags, media.Tag{Key: string(tag.Title), Value: v.Title})
	}
	if v.Author != "" {
		tags = append(tags, media.Tag{Key: string(tag.Artist), Value: v.Author})
	}
	if !v.PublishDate.IsZero() {
		tags = append(tags, media.Tag{Key: string(tag.RecordingDate), Value: v.PublishDate.Format("2006-01-02")})
	}
	return tags
}

// muxEmbedLossDetail renders the WarnTagCarry detail for source metadata that
// could not ride a mux-embedded output, or "" when nothing was lost. carried
// is how many text tags did ride the encode: with none, the message must not
// claim a carry that never happened (a source whose only metadata is a
// picture).
func muxEmbedLossDetail(dest string, format media.Codec, lost []string, carried int) string {
	if len(lost) == 0 {
		return ""
	}
	what := fmt.Sprintf("%s cannot be embedded in a %s file", strings.Join(lost, ", "), format)
	if carried > 0 {
		return fmt.Sprintf("metadata carried to %s with losses: %s", dest, what)
	}
	return fmt.Sprintf("no metadata carried to %s: %s", dest, what)
}

// warnMuxEmbedLosses emits muxEmbedLossDetail for one output; the album path
// aggregates the same detail across tracks instead (albumCarryLoss).
func warnMuxEmbedLosses(em *emitter, dest string, format media.Codec, lost []string, carried int) {
	if detail := muxEmbedLossDetail(dest, format, lost, carried); detail != "" {
		em.warn(WarnTagCarry, detail)
	}
}

// warnMuxEmbedRequests reports the parts of a download's embed request that a
// mux-embedded output cannot honor: cover art has no APEv2 form here, and
// chapters have no form at all. The text tags themselves (when requested) rode
// the encode.
func warnMuxEmbedRequests(em *emitter, dest string, format media.Codec, v *youtube.Video, o embedOptions) {
	var lost []string
	if o.thumbnail {
		lost = append(lost, "cover art")
	}
	if o.metadata && v != nil && len(v.Chapters) > 0 {
		lost = append(lost, "chapters")
	}
	if len(lost) == 0 {
		return
	}
	detail := fmt.Sprintf("%s cannot be embedded in a %s file", strings.Join(lost, " and "), format)
	if o.metadata {
		detail += "; " + dest + " carries the text tags only"
	}
	em.warn(WarnMetadataEmbed, detail)
}
