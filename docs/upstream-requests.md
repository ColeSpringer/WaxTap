# Upstream requests

The standing list of things WaxTap wants from the sibling Wax repos it
depends on: WaxFlow and WaxLabel, the Go modules in `go.mod`, and WaxSeal,
the sidecar behind the player-context and PO-token contracts. Every entry
is a candidate for whenever upstream work is next scheduled; nothing here
implies timing, and none of it blocks WaxTap, since each entry names the
workaround WaxTap ships today. Agents: when you defer something because it
needs upstream support, add it here in the same change and put the
WaxTap-side follow-up in [deferred-work.md](deferred-work.md); when
upstream lands it, do the follow-up and remove both entries.

## WaxSeal

- **The player context carries only a title, an author, and a length.**
  `/player-context` answers `title`, `author` and `length_seconds` from
  the browser's `videoDetails` and nothing else, so the `Video` WaxTap
  builds on the WEB_CONTEXT path (`youtube/web_context.go`) has no channel
  ID, description, thumbnail ladder, publish date, or live state, where
  every `/player` path fills them from the same `videoDetails`. A download
  delivered over WEB_CONTEXT therefore reports `Result.Metadata` with an
  empty `ChannelID` and `Description`, and `--write-info-json` omits
  `channelId` and `description` on that path alone, while `Video.ChannelID`
  is documented as the canonical identity anchor. Wanted: `channel_id`,
  `description`, the `thumbnails` ladder (url, width, height), the live
  flags (`is_live_content`, `is_live_now`, `is_upcoming`) and
  `publish_date` on the response, copied from the `videoDetails` and
  microformat the attesting browser already holds. Shipped workaround:
  none is needed for `Info` and `Enumerate`, which never take that path;
  cover art on that path comes from WaxTap's own ID-keyed thumbnail
  probes, and the watch-page pass backfills the publish date, chapters,
  and availability when a request asks for full metadata. The WaxTap-side
  mapping is in deferred-work.md.

## WaxFlow

- **The album timeline refuses the frame tail its own WMA decoder
  delivers.** `Concat` fails a member that delivers past `Track.Samples`
  ("timeline member N holds more audio than the M samples its headers
  declared"), and the WMA decoder always delivers whole frames past the
  declared length because WMA has no padding count. So `normalize --album`
  and `MeasureAlbum` on any WMA input exit 2. Wanted: the bound to
  tolerate a tail when `SamplesExact` is false, or the decoder to trim to
  the declared length. Shipped workaround: WaxTap rewords the refusal
  ("album mode cannot take a WMA file yet; process WMA tracks one at a
  time"), documents it in the README, and pins it with
  `TestProcessAlbumWMAIsUpstreamLimited`, which fails the day an album of
  WMA succeeds so the wording and the note can go.

- **One code for two refusals.** `waxerr.CodeUnsupportedFormat` marks both
  an encoder refusing a spec and a container rejecting malformed bytes.
  WaxTap maps it to the invalid-spec sentinel (exit 2) and can only
  override that at sites known to be reading (`classifyInputError` in
  `internal/media/errors.go`), so malformed input reaching a site that
  both reads and encodes reports as a bad request. Wanted: a distinct code
  for one of the two. Shipped workaround: the read-only-site override.

- **Container warnings carry no severity.** `container.Warning` is an
  offset and a message, and demuxers record tolerated damage and notes
  about files that play fine (an ignored extra stream, a skipped trailing
  tag, a rescaled timescale) through the same type. WaxTap's
  `input-damage` warning therefore relays the notes in the decoder's words
  with no lead that claims damage, because "the source is damaged" was
  true of half of them. Wanted: a severity or kind on the warning so the
  two can be told apart. Shipped workaround: the verbatim relay in
  `inputDamageNote` (`mapping.go`).

## WaxLabel

- **A transfer cannot take remapped chapters.** `PrepareTransfer` carries
  a source's chapters as they are, and a `Document` is a projection of a
  file, so a cut that moves or drops chapters costs a second metadata
  rewrite: WaxTap transfers, then re-edits the post-write document with the
  remapped set (`rewriteChapters` in `carrytags.go`). Wanted: a way to hand
  the transfer a replacement chapter list so the write lands once. Shipped
  workaround: the second rewrite, which is correct and only slower.
