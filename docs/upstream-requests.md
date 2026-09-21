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

- **A lazy open on `GroupMember`, the way `ConcatSource` has one.**
  `Engine.AnalyzeGroup` takes `[]GroupMember{Media: format.Media}`, an
  already-open source the caller owns, so a caller must open every member
  before the call and hold each descriptor and demuxer state until it
  returns. `ConcatSource` solved the same problem with an `Open func()
  (format.Media, error)` the timeline calls as it reaches each member, which
  let the pass it replaced hold one descriptor at a time. An album of N
  tracks is now N open files at once.

  *Workaround WaxTap ships:* `media.Runner.AnalyzeGroup` opens all members up
  front and closes them together. Album sizes in practice stay well under any
  descriptor limit, so this is a cost rather than a failure.

## WaxLabel

No open requests.
