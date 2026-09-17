# Deferred work

The tracked list of WaxTap work that was cut from an otherwise shipped
change, that waits on a sibling repo, or that a sibling repo's release made
possible and WaxTap has not taken up. Work that never started for any other
reason does not belong here; this list is for residuals that would otherwise
survive only as a sentence in a plan or a progress note. Agents: when you
cut something, or a dependency bump brings a capability you leave unbuilt,
add it here in the same change; when it lands, remove the entry. Asks of
the sibling repos live in
[upstream-requests.md](upstream-requests.md), and an `[upstream]` entry
here names the ask it waits on.

Gate tags:

- `[in-repo]` nothing blocks it; it was cut for scope and is ours to
  build when picked up.
- `[upstream]` needs sibling-repo work first; the ask is in
  upstream-requests.md.

## Enumeration and metadata

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

- `[in-repo]` **Normalize Opus by its header gain instead of re-encoding.**
  WaxLabel 1.7.0 reads and writes the `OpusHead` output gain
  (`Editor.SetOutputGain`, signed Q7.8 dB, with the RFC 7845 rebase of
  `R128_TRACK_GAIN`/`R128_ALBUM_GAIN`), and every compliant decoder applies
  it, WaxFlow's included. `normalize` on an Opus source that stays Opus
  decodes and re-encodes today, a generation of loss for the format most
  YouTube downloads arrive in. In `--peak-mode cap` the gain is one scalar
  (`loudness.GainFor`, held under the true-peak ceiling), which the header
  field carries exactly, so the run could remux the packets untouched and
  patch page 0; album mode's uniform gain fits the same way. `limit` cannot
  take this path, since the limiter reshapes samples. Undecided: whether it
  is the default for Opus-to-Opus (a behavior change: the delivered samples
  no longer carry the gain, and a player that ignores the field plays the
  old loudness) or opt-in behind a flag, and how the delivered measurement
  reports a gain no sample was changed by. Surfaced by the 2026-09-16
  WaxLabel bump.

- `[in-repo]` **Split a single-file rip by its CUE sheet.** WaxFlow
  published its `cue` package with the 2026-09-16 bump (`cue.Parse` reads a
  sheet, `File.Starts` validates the track boundaries, in CD frames of
  1/75 s). WaxTap has the pieces a split needs, the cut path and the carry,
  but no flow that takes a sheet: it would be a new subcommand, or a `cut`
  mode, that cuts one file into N outputs at the sheet's INDEX 01 positions,
  names them from TITLE and PERFORMER, and writes those as tags. Undecided:
  the command's shape, how disc-level metadata (CATALOG, REM DATE) maps onto
  tags, and whether a pregap (INDEX 00) belongs to the previous track.
  Nobody has asked for it.

- `[in-repo]` **A cut trusts a header that overstates the length.** The
  pipeline resolves cut ranges against the probe's duration, which for a
  lazily walked payload (MP3, bare or in a WAV or AIFF-C; ADTS) is the
  header's claim (`internal/pipeline/pipeline.go`, the probe stage). A
  truncated MP3 still declares its Xing count, so a span that runs to the
  declared end outruns the file and the engine refuses the run ("the source
  ended N samples into a span that declared M; its cut points do not
  describe this file", which WaxTap reports as an I/O failure, exit 10),
  where a truncated FLAC clamps at the probe and the cut runs against the
  real length. Fix: for a cut, walk such an input first (`format.Walker`,
  which a strict probe runs) so the total is measured, and resolve the
  ranges against that. Found by the 2026-09-16 review round.

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

- `[in-repo]` **The `cmd/waxtap` suite depends on test order.** `go test
  -shuffle=on ./cmd/waxtap` fails on a different test each run (seen:
  `TestBatchTranscodeCommandIntegration`, `TestBatchDownmixIsDecidedPerFile`,
  each on which batch item was copied rather than encoded), so some state
  one test sets outlives it. CI runs the suite in source order until it is
  found; add `-shuffle=on` to the test job's `go test` once a shuffled run
  passes. Found reworking the workflows on 2026-09-17.

## CI

- `[in-repo]` **The daily `doctor` run has never seen past the bot wall.**
  Not one of the 105 runs of `.github/workflows/doctor-cron.yml` since its
  first on 2026-06-04 has passed: each ran to the end of its retry loop (199
  to 289 s, where a first-attempt pass returns within a minute), and every
  log that survives, 2026-06-20 on, shows `login-required` on all three
  candidates from the GitHub-hosted runner's address, classed as
  environmental, so the job stayed green throughout. The refusal arrives in
  the player response, before there is anything to descramble, so the
  workflow's one hard signal, an exit 4 from the extraction or cipher path,
  has had nothing to observe. The 2026-09-17 rework puts each run's verdict
  on its summary page and reads every candidate of every attempt for that
  class, which shows the streak but does not end it. Ending it means running
  from an address YouTube serves: a self-hosted runner, `--proxy` to one, or
  the sidecar URLs (`--session-url`, `--potoken-url`,
  `--player-context-url`) from repository secrets, which is what `doctor`
  probes for. Undecided: which, and whether a run that never reaches YouTube
  should keep counting as green. Found reworking the workflows on
  2026-09-17.
