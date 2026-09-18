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

## Processing

- `[upstream]` **`info --probe` stages the whole stream.** `probeRemote`
  (`waxtap.go`) downloads the audio once to probe a header locally. A lazy
  ranged-HTTP `container.Source` (WaxFlow's `container.Contextual` and
  `BindContext` exist for exactly that; the googlevideo dialect is
  `download.QueryRange`) would not save the read for the row YouTube serves
  by default: WaxFlow walks every cluster of a WebM Opus track at open, so a
  probe reads the file either way, and only an m4a row would benefit. Waits
  on the WaxFlow header-only probe ask in upstream-requests.md; the
  follow-up is a ranged source bound with `BindContext` in
  `media.Runner.probeSource`, once a probe is header-sized. An accepted
  tradeoff since v3.0.

- `[upstream]` **An album mixing a surround member with a stereo one cannot
  be measured as a group.** `loudness.MeasureAlbum` measures the group over
  a WaxFlow Concat, which conforms every member to the widest layout through
  `dsp/mix`, and that mixer builds no target wider than stereo, so the
  timeline fails when it reaches the narrower member, with an error that
  names no track. WaxTap therefore reads every member's width first and
  refuses such a set before measuring, naming the narrower track
  (`normalize --album` and `--album --measure-loudness` both). The
  per-member fold (this sweep) covers the uniform case, every member the
  same width. Waits on the WaxFlow Concat ask in upstream-requests.md; the
  follow-up is a group pass over per-member folded media once one exists.

- `[upstream]` **A truncated ADTS's walked count can overstate a genuinely
  truncated final frame.** `adts.Demuxer.extend` indexes a resynced frame
  from its header alone and only discovers, one call later, that its
  declared span runs past the data end, by which point it is already
  counted, with no warning attached (the ADTS walker ask in
  upstream-requests.md). `Runner.MeasureLength`'s walk path has no signal
  to detect or correct this, so `TestMeasureLengthOfTruncatedPayloads`
  tolerates a one-frame gap against an independent decode instead of
  asserting equality, and `CutSpec.SourceSamples` can be a frame too
  generous for a bounded (non-open-ended) span on such a file, or for a
  multi-keep composition's open-ended final span, which still declares
  `SourceSamples - from` at `Concat`'s seam (a single-keep open-ended span
  opens through `Slice` instead and is genuinely unaffected). What that
  costs a user: an interior cut of a truncated ADTS (a multi-keep
  composition, which is what removing a span in the middle composes) is
  refused as unsupported input, where the same cut of a truncated MP3 now
  runs; `TestCutOnTruncatedLazyPayloadMeasuresFirst` asserts the ADTS case
  only for a span that reaches the end. Waits on the WaxFlow ADTS walker ask
  in upstream-requests.md; the follow-up is tightening that test back to
  strict equality, asserting the interior cut, and dropping the one-frame
  slack the `SourceSamples` bound carries for it.

## Tests

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
