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

## Open entries

- `[in-repo]` **A SABR round whose body stalls ends the stream.** The stall
  timeout (2026-11-01) retries a round the server never answered, through
  the HTTP client's transport ladder, but a round whose body stops part way
  fails the stream with the stall (exit 9 `timeout`): `sabr.stream.fill`
  treats any round error as terminal, where a direct chunk is resumed.
  Re-POSTing a request the server already began answering has not been
  checked against a live server, where it might be read as a seek or reset
  the playback cookie. Worth one bounded retry once a live run can confirm
  the server's answer.
