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

- `[upstream]` **A mixed-fold album renders temporary PCM copies to measure
  its group.** WaxFlow took the up-mix half of the Concat ask and not the
  per-member fold, so `loudness.MeasureAlbum` cannot ask a Concat to fold each
  member at its own width: the timeline conforms every member to the widest
  layout first, and `dsp/mix` normalizes each output row by the energy of every
  source coefficient, silent positions included, so a fold applied after that
  widening is about 1 dB off the member's own fold. When a set's folds or
  source widths differ (a surround member folded to stereo for a lossy target
  beside a stereo one), WaxTap therefore renders every folding member to a
  temporary PCM WAV at its fold and runs the group pass over that set
  unfolded. Cost: one decode and one PCM write per folding member, on that
  arm only. Waits on the per-member Concat fold ask in
  upstream-requests.md; the follow-up is deleting `groupPass`'s rendering arm
  and passing the folds to `Concat` instead.

- `[upstream]` **`info --probe` reports no length for a fragmented MP4.**
  YouTube's itag 140 is fMP4: a 699-byte init `moov` with an empty sample
  table, a `sidx`, then one `moof`+`mdat` pair per ~160 KB of audio.
  WaxFlow's `mp4.Demuxer.fragmentedGapless` takes a fragmented track's length
  from the init segment's edit list alone and leaves it unknown when there is
  none, and YouTube writes no `edts`/`elst`, so the probe reports a zero
  duration for that row while its `mvhd` states the real one and the `sidx`
  beside it carries every fragment's duration. Measured on a ten-minute track:
  the probe reads to the end of the fragments looking for what it never finds,
  which was 39 range requests over the whole 10 MB before `probeRangeBudget`
  bounded it to 1 MiB and sent the row to the staging path instead. The answer
  is the same zero either way; the bound costs the 1 MiB already fetched on top
  of the staged download, about 10% for that row, and it is what keeps a probe's
  cost a property of the design rather than of the container. Waits on the
  WaxFlow fragmented-length ask in
  upstream-requests.md; the follow-up is asserting a probed duration for the
  itag-140 row and, once a fragmented open is header-sized, dropping that row
  from what the budget has to cover.
