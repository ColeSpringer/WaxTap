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
  Shipped workaround: none; the message names the cause, and the
  WaxTap-side fix that avoids the refusal for the common case (measuring
  the input before a cut) is in deferred-work.md.

## WaxLabel

No open requests.
