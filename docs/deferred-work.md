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

- `[upstream]` **An album measurement holds one descriptor per track.** The
  Concat pass `AnalyzeGroup` replaced held one at a time by design;
  `waxflow.GroupMember` takes an already-open `format.Media`, so every member
  is opened before the call and held until it returns. Waits on the lazy-open
  ask in [upstream-requests.md](upstream-requests.md); when it lands,
  `media.Runner.AnalyzeGroup` opens each member as the engine reaches it.
- `[in-repo]` **A dead proxy still costs two dial timeouts, not one.** CLI-6's
  fix bounds every dial at 10 s while a proxy is configured, stops retrying a
  `proxyconnect` failure, and stops the extraction chain on one, which took the
  default-budget cost from 45 s to 20 s with the proxy named. The remaining 10 s
  is the session bootstrap, which swallows its own failure by design
  (`newBootstrappedSession` "never errors" outside adoption). Making it report a
  proxy failure would change that contract, which is wider than this finding, so
  it was left. Picking it up means deciding what a bootstrap that cannot reach
  the network should do to the run.
- `[in-repo]` **An extension-named output re-encodes a URL it could have
  copied.** The container rule reads the output extension, asks
  `media.OutputCodecFor` what that container carries, and passes the answer as
  the transcode format. A local file is probed first, so a container that
  accepts the source codec yields a copy; a URL is not, so `OutputCodecFor`
  answers from the container alone and `transcode <url> out.mka` re-encodes
  Opus to Opus. The wrong message this produced is fixed, since
  `warnImplicitLossy` now asks `media.ContainerAccepts` rather than trusting
  how the request was built, so what is left is a silent second generation.
  Picking it up means letting
  the pipeline decide, with a "the container chose" bit on `pipeline.Spec` so
  the promotion runs against the staged source's real codec and one rule covers
  both. `--force` and any honoured knob still have to mean an encode.
- `[in-repo]` **A copy into ADTS drops the exact length.** `transcode x.m4a
  out.aac` copies the AAC frames untouched, but ADTS states no sample count:
  the 10 s source probes at `samples=441000` and the copy at `samples=-1`, so
  the encoder delay and tail padding the MP4's gapless trim hid become audio.
  Raw ADTS has no field for it, so this is not an upstream ask; it predates
  this change and applies to every cut and copy landing in such a container.
  Picking it up means saying so, as a warning when the output container cannot
  carry a gapless trim the source stated.
