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

- **A total of the samples a read filled with silence.**
  `container.Packet.Lost` says how many samples a hole took before a packet
  and `format.Media` delivers that many of silence, but nothing a caller of
  `Engine.Transcode` or `Engine.Analyze` holds says how much of the delivered
  stream was fill: `format.Info.Damage` counts findings, and the per-hole
  count is only in the warning's text ("4096 samples lost"). WaxTap refuses
  an album track whose read covers materially less than its declared length,
  since the shared gain would describe part of a track, and a FLAC with a
  hole reads to its full length, so the length no longer says how much was
  missing. Wanted: the lost-sample total on `format.Info` beside `Damage`,
  or on `TranscodeResult` and `AnalyzeResult`.

  *Workaround WaxTap ships:* `media.LostSamples` sums the counts out of the
  read's warning text, and an album refuses a track that lost more than half
  a second. The text is not a contract, so a rewording reads as nothing lost,
  and the warning list's cap of 64 undercounts a file damaged in more places.

## WaxLabel

No open requests.
