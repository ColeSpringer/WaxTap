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

- **The retryable refusals carry no `Retry-After`.** `player-context-failed`
  (502, the 30 s proof cool-down) and `no-session` (503, session
  re-establishment) are documented retryable, but the header is sent only
  on `/report` today, so a consumer has to guess how long the cool-down
  has left. WaxTap now honours `Retry-After` and the body's
  `retry_after_seconds` on both refusals: one retry after the stated wait,
  capped at 60 s, and the wait also sets the WEB-context skip window.
  Wanted: send the remaining cool-down on both, which WaxSeal's own
  deferred item already gates on a consumer honouring it. Shipped
  workaround: one 500 ms retry, then the fallback chain, and the default
  30 s skip window.

- **`/player-context` carries `client_version` but no `user_agent`.** The
  context arm streams under WaxTap's own Chrome user agent with the
  context's client version and visitor id, so the identity the URL was
  minted under and the identity the stream presents differ in the one
  header a browser is most recognisable by. The `/session` contract now
  exports both and WaxTap adopts them; `webContextProfile`
  (`youtube/web_context.go`) would apply a `user_agent` the same way.
  Wanted: `user_agent` beside `client_version` on the response. Shipped
  workaround: none needed, delivery is full length under WaxTap's
  identity.

- **A bot check is answered as a per-video verdict.** `confirmTerminal`
  turns any non-OK playability status into `video-unavailable`, so a
  browser hit by "Sign in to confirm you're not a bot" refuses every video
  with `LOGIN_REQUIRED`, which WaxTap classifies as login-required per
  item with no cool-down: a batch pays one context call per item until the
  chain delivers. The verdict is dropped once another client reaches the
  stream, so it misreports an item only when nothing delivered. WaxSeal
  holds the reason text and knows the difference.
  Wanted: answer a bot check as `player-context-failed` with a
  `Retry-After` and relaunch the session, as it does for a failed proof.
  Shipped workaround: the chain delivers through a native client and each
  item pays one context call.

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
  Shipped workaround: WaxTap measures an input whose length is only claimed
  before resolving a cut (a walk, or a decode for a Xing MP3 and a WMA; see
  the walk entry below), so the refusal is unreachable for the common case;
  the code is still wrong for the rest.

- **A probe of a WebM Opus track reads the whole file.** `mka.finalizeTrack`
  (`container/mka/demux.go:600`) calls `ensureWalk`
  (`container/mka/read.go:311-314`) at open for a track with a nonzero
  CodecDelay, which every Opus rendition YouTube serves carries (and for
  Vorbis, `needsGaplessWalk`, `demux.go:641-643`), once the first cluster is
  seen. The walk frame-counts every cluster through the 128 KiB read-ahead
  window (`container/internal/srcwin/srcwin.go:31`). `Engine.Probe` on
  YouTube's default Opus rendition is therefore a linear read of the stream,
  and a ranged-HTTP `container.Source` cannot make `info --probe` cheap: it
  would fetch the same bytes in `size/128 KiB` requests. Only an m4a row
  probes from its `moov`. Wanted: a probe option that reports the advisory
  `Info` Duration for such a track without the walk, with the count marked
  `SamplesAdvisory`, so a header-only probe is a header-sized read. Shipped
  workaround: `probeRemote` (`waxtap.go`) stages the whole stream to a temp
  file and probes that.

- **A Concat cannot conform a narrower member into a surround envelope.**
  `concatLayout` promises to mix a member up to
  `audio.DefaultLayout(env.Channels)` (`timeline.go:716-718`), but
  `dsp/mix.For` builds mono to stereo, anything to stereo, and anything to
  mono only (`dsp/mix/mix.go:71-113`), and `concat.buildChain` asks it for
  the envelope layout when the timeline reaches the member
  (`timeline.go:1486-1500`), so a stereo track queued with a 5.1 one fails
  there with unsupported-format, and the error comes back bare, with no
  member number to name the track by. WaxTap's album measurement runs over a
  Concat, so an album mixing widths cannot be measured. Wanted: either an
  up-mix to the conventional wider layouts (zero-filled positions, no
  normalization), or a `ConcatOptions` fold that conforms and measures every
  member at one width, which is also what an album encode into a lossy
  target needs measured. Shipped workaround: WaxTap reads every member's
  width first and refuses the set before measuring, naming the narrower
  track.

- **A walk that comes up short keeps the Xing count.** `mpa.Walk`
  (`container/mpa/index.go:22-31`) adopts the walked frame count only for a
  track that stated none or an advisory one; a count a Xing or Info frame
  stated stands even when the walk found fewer frames, so a strict probe of
  a truncated MP3 reports the declared length beside a "truncated final
  frame dropped" warning, and the length a cut can trust is only learned by
  decoding to EOF. ADTS, MP3 in a WAV or AIFF-C, and non-Opus Matroska are
  settled by the walk alone. Wanted: a walk that ends short of the stated
  count replaces it, marked as damage, so the walk is the measurement for
  MP3 too. Shipped workaround: WaxTap walks a lazily walked input before
  resolving a cut and decodes it only when the walk could not settle the
  count, which is the Xing case; a normalizing cut of such a file then
  decodes twice (the count, then the measurement of the composed cut), and a
  copy cut of ADTS walks once more than the packet grid already does. The
  decode is paid on a healthy file too, which is the part this ask removes:
  the walk's damage warning cannot gate it, since a Xing MP3 truncated
  exactly on a frame boundary walks clean while still declaring its full
  count (measured: an 803525-byte fixture cut to 481697 bytes walks with no
  warning, declares 882000 samples, and decodes 528863), so the only way to
  tell that file from an intact one is to decode it. Cost: about 36 ms per
  20 s of audio on a warm cache, on every cut of every Xing MP3, copy cuts
  included.

- **A truncated ADTS frame can be walked in as if it were whole.**
  `adts.Demuxer.extend` (`container/adts/demux.go:294-322`) resyncs to a
  candidate frame and appends it to the index (`:324`) without confirming
  its declared span (`candidate + frameLen`) actually fits inside
  `DataEnd()`. Only the *following* call re-derives that same frame as
  `last` and finds `next >= DataEnd()` (`:290`), by which point it is
  already indexed and there is nothing left to flag: no warning, no count
  correction. `adts.Demuxer.Walk` (`demux.go:342`) then reports
  `len(idx)*spf` as the exact sample count, one frame too many. Measured
  over 1200 truncation offsets on a synthetic fixture: the walked count and
  an independent decode differ by exactly 1024 samples (one AAC-LC frame)
  in 1179 cases, and agree in the other 21 (a cut landing on a real frame
  boundary, or inside a following frame's own header, both of which the
  resync path does catch). `mpegframes.Walker.extend`
  (`container/internal/mpegframes/walker.go:335`, shared by mpa/riff/aiff -
  bare MP3, MP3 in a WAV, MP3 in an AIFF-C) already carries the check ADTS
  lacks, `if cand+nh.Size() > DataEnd() { ...warn... }` before its own
  `idx = append` (`walker.go:379-383`), and `Begin` guards the first frame
  the same way, so this is an ADTS-only gap: over the same 1200 offsets on
  an MP3 fixture, 1198 warn and `MeasureLength` matches an independent
  decode at all 1200 (see the entry above for the one MP3 case that does
  mismeasure, a stale Xing count, unrelated to this walk). Wanted: `extend`
  confirming a resynced candidate's declared span is inside `DataEnd()`
  before appending it, the check `mpegframes` already makes. Shipped
  workaround: none functional; WaxTap's own test for
  `media.Runner.MeasureLength` (`TestMeasureLengthOfTruncatedPayloads`)
  tolerates a one-frame gap against an independent decode on the walked
  path rather than asserting equality, since `MeasureLength` has no signal
  to detect or correct this (the walked `Info`'s own Warnings are empty
  too). A cut's own bound (`CutSpec.SourceSamples`) can therefore still be
  a frame too generous for a bounded (non-open-ended) span on a truncated
  ADTS file. The open-ended form a span takes when it reaches the measured
  total (`openComposed`'s `openEnded`) is unaffected only for a single-keep
  composition, which opens through `Slice`: its open-ended form makes no
  length promise at all. A multi-keep composition opens through `Concat`
  instead, and WaxFlow's `SpanTrack` (`timeline.go:259-267`) still declares
  the open-ended final member's length as `SourceSamples - from`, so
  `Concat`'s seam check refuses it by the same one frame.

## WaxLabel

No open requests.
