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

- **A span that outruns its source is refused as unreadable.** `Slice`
  fails a read that reaches the end of the source inside a span with
  `CodeSourceUnreadable` ("the source ended N samples into a span that
  declared M; its cut points do not describe this file", `timeline.go`),
  where the split that added `CodeMalformedInput` puts a file ending short
  of what it declares under the new code (`container.ShortRead`'s own rule:
  a source that ended early is damage, not a fetch that failed). WaxTap maps
  the unreadable code to an I/O failure (exit 10), so a cut of a truncated
  MP3 reports a bad disk. Wanted: `CodeMalformedInput` on that refusal.
  Shipped workaround: none; the message names the cause, and the
  WaxTap-side fix that avoids the refusal for the common case (measuring
  the input before a cut) is in deferred-work.md.

## WaxLabel

No open requests.
