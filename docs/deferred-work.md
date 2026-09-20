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

## Doctor

- `[in-repo]` **`doctor` does not ask WaxSeal's `/ping` before paying for a
  proof.** WaxSeal answers `GET /ping?strict=true` in one round trip with a
  `reason` (`ok`, `no-session`, `busy`, `probe-failed`) and, keyless on a keyed
  daemon, a daemon-scope answer that needs no key; `probeSidecars`
  (`cmd/waxtap/doctor.go`) probes the session, token, and context endpoints
  with browser-backed calls only, so a walled or wedged daemon is reported as
  a `502` with no reason. Cut from the 2026-09-19 contract pass for scope; the
  follow-up is a cheap `/ping` probe ahead of the three, shown with its reason.
