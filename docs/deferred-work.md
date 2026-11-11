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

- `[upstream]` **The size of a hole is read out of a warning's text.** Since
  WaxFlow `2f448c9` a FLAC that lost frames, native or in Ogg, reads to its
  full length with silence where they were, so a length no longer says how
  much of a track a measurement covered. An album refuses a track that lost
  more than half a second this way, as it refuses a short decode, and the
  only place the engine states the count is the finding's wording
  (`media.LostSamples`, "... 4096 samples lost"). A reworded warning reads
  as nothing lost, and a file damaged in more places than the engine lists
  (64) is undercounted. Take the count from a field when one exists
  (upstream-requests.md, WaxFlow) and drop the parse.
