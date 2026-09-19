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

No open requests.

## WaxFlow

- **A Concat cannot fold each member at its own width.** The up-mix half of the
  earlier ask landed (`mix.For` places each source position at unity and
  zero-fills the rest, and `Concat` widens every narrower member), so an album
  mixing widths measures as a group now. The other half did not: there is no
  `ConcatOptions` fold, and a fold applied to the assembled timeline is not the
  member's own, because `dsp/mix` normalizes each output row by the energy of
  every source coefficient, silent positions included
  (`dsp/mix/mix.go:152-165`), so a 5.1 member widened to 7.1 and then folded to
  stereo lands about 1 dB under its direct fold. An album encoded into a lossy
  target needs every member measured at the width its own encode delivers.
  Wanted: a `ConcatOptions` fold that conforms and measures each member at its
  own width, applied before the member is conformed to the envelope. Shipped
  workaround: when a set's folds or source widths differ, WaxTap renders every
  folding member to a temporary PCM WAV at its fold and concatenates that set
  unfolded (`loudness.groupPass`), at one decode and one PCM write per folding
  member.

- **A fragmented MP4 with no edit list reports no length.**
  `mp4.Demuxer.fragmentedGapless` (`container/mp4/fragdemux.go:83`) resolves a
  fragmented track's length from the init segment's `edts`/`elst` and returns
  `samples = -1` when the track has none ("the length is left unknown and the
  track decodes to end of stream"). YouTube's itag 140 has none: its init
  `moov` is `mvhd` + `mvex` + a `trak` whose `stbl` is empty, and the timing
  lives in the 64 `moof` boxes spread across the file. Two things beside the
  moov already state the length: `mvhd`/`mdhd` carry
  `duration=27986944 @ timescale 44100` (the exact 10:35 of the track), and the
  `sidx` immediately after the moov lists every fragment's duration and size in
  800 bytes. Wanted: fall back to the `sidx`'s summed durations, or to the
  movie header's, when a fragmented track has no edit list, so a fragmented
  open states a length and is header-sized. Shipped workaround: `info --probe`
  reports a zero duration for that row and falls back to the manifest's
  `approxDurationMs`; WaxTap bounds the ranged scan it provokes with
  `probeRangeBudget` (1 MiB) and stages the stream once past it, so the row
  pays the 1 MiB on top of the download rather than fetching the file one
  block at a time.

## WaxLabel

No open requests.
