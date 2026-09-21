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

- `[in-repo]` **A format-named extension still re-encodes a URL it could have
  copied.** The container rule now runs against the staged download
  (`pipeline.Spec.ContainerChosen`), so `transcode <url> out.mka` copies an
  Opus delivery. It never reaches an extension that names a format rather than
  a container: `inferOutputFormatProbed` answers such a name from the name
  alone (`.opus`, `.aac`, `.mp3`), leaves `FromContainer` false, and the
  pipeline encodes, so `transcode <url> out.opus` on an Opus delivery is a
  second generation nobody asked for. A local file is spared by the CLI's
  same-format shortcut, which probes first; a URL cannot be probed before the
  download. Picking it up means deciding what the extension means for a source
  nobody has seen yet, since `.opus` names an encoder in a way `.mka` does not,
  and `implicit-lossy` must not start firing on a request that named its
  format.
- `[in-repo]` **A cut that falls back to a same-family re-encode says
  nothing.** WaxFlow packet-cuts Opus and AAC; every other lossy source
  (`cut in.mp3 out.mp3`, `cut in.ogg out.ogg`) declines the copy cut and
  renders the cut in the source's own family, which is a second lossy
  generation. `implicit-lossy` does not cover it: that warning asks whether
  the output container could carry the source codec, and here it can, so the
  early return fires and the run reports only `transcoded: true`. It predates
  the container rule and is not part of it. Picking it up means a report for
  "the cut had to decode", which is a different fact from "the container chose
  the encoder" and may want its own code.
