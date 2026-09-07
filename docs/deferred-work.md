# Deferred work

The tracked list of WaxTap work that was cut from an otherwise shipped
change, or that waits on a sibling repo. Work that never started does not
belong here; this list is for residuals that would otherwise survive only
as a sentence in a plan or a progress note. Agents: when you cut
something, add it here in the same change; when it lands, remove the
entry. Asks of the sibling repos live in
[upstream-requests.md](upstream-requests.md), and an `[upstream]` entry
here names the ask it waits on.

Gate tags:

- `[in-repo]` nothing blocks it; it was cut for scope and is ours to
  build when picked up.
- `[upstream]` needs sibling-repo work first; the ask is in
  upstream-requests.md.

## Enumeration and metadata

- `[upstream]` **Fill the web-context `Video`.** Once WaxSeal's
  `/player-context` carries the channel ID, description, thumbnail
  ladder, publish date, and live flags (upstream-requests.md, WaxSeal),
  map them through `playerContextResponse` (`sidecar.go`),
  `potoken.PlayerContext`, and `ExtractWebContext`
  (`youtube/web_context.go`), so `Result.Metadata` and
  `--write-info-json` carry them on that path as on every other. The
  empty-ladder note on `minProbeArea` in `thumbnail.go` describes the
  path as it is today and needs rewording then.

- `[in-repo]` **A live or upcoming marker on the listing entry.** The
  legacy renderer's thumbnail overlay carries a LIVE or UPCOMING
  time-status style, and the lockup's badge text is what
  `parseBadgeDuration` already sees and rejects, but `PlaylistEntry` has
  no field for either. A consumer cannot tell a live item from a video
  with an unknown length without a fetch, and a budgeted `Enrich` spends
  a request on an entry `Info` is certain to refuse with `ErrLiveContent`
  or `ErrLiveNotStarted`. Not asked for; noted while doing the 2026-09-06
  overlay change.

## Processing

- `[in-repo]` **Loudness is measured at the source width while the
  encode folds.** `internal/pipeline/pipeline.go` measures the input with
  `fold` set only by an explicit `Downmix`, but opus always folds a wide
  source to stereo and mp3 and aac have since the 2026-08-15 WaxFlow
  bump, so the gain is computed for audio the encoder never meters, and
  folding changes both true peak and integrated loudness. Fix: key the
  input measure on the resolved output width, from the per-codec fold
  policy or a WaxFlow plan query. Reachable only on a source wider than
  stereo going into a lossy format without `--downmix`; the
  `implicit-downmix` warning names the fold after the fact.

- `[upstream]` **Retire the WMA album pin.** When WaxFlow's timeline
  tolerates the WMA frame tail (upstream-requests.md, WaxFlow),
  `TestProcessAlbumWMAIsUpstreamLimited` fails; delete it, the rewording
  in `albumTrackError` (`process.go`), and the README note.

- `[upstream]` **Map encoder refusals apart from malformed input.** When
  WaxFlow splits `CodeUnsupportedFormat` (upstream-requests.md, WaxFlow),
  map the encoder half to `ErrIncompatibleSpec` and the malformed half to
  `ErrUnsupportedInput` in `internal/media/errors.go`, and retire the
  read-only-site override in `classifyInputError`.

- `[upstream]` **Use a warning severity for `input-damage`.** When
  `container.Warning` gains one (upstream-requests.md, WaxFlow), lead the
  note with a damage verdict for the damage class only (`inputDamageNote`
  in `mapping.go`).

- `[in-repo]` **`info --probe` stages the whole stream.** `probeRemote`
  (`waxtap.go`) downloads the audio once to probe a header locally, which
  sidesteps ranged-HTTP probing and header-size cliffs. A lazy
  ranged-HTTP source is the optimization if the cost ever bites. An
  accepted tradeoff since v3.0.

## Tests

- `[in-repo]` **The probe back-fill has no offline test.** `InfoResult`
  fills a still-zero `Video.Duration` from the probed row after
  `applyProbe`; only the live `TestLive_InfoProbe` reaches it, because
  probing stages a real stream.
