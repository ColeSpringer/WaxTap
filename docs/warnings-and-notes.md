# Warnings and notes

Warnings are conditions the library observed; notes are decisions the CLI
made. In human output both print to stderr, where `--quiet` drops the notes.
In `--json` they are `warnings[]` and `notes[]`, each a `{code, detail}`
object.

| Warning | Meaning |
|---|---|
| `proceed-uncut` | the SponsorBlock fetch failed; the audio was delivered uncut |
| `sponsorblock-empty` | SponsorBlock matched no segments |
| `ranges-empty` | every SponsorBlock segment fell outside the media |
| `fallback-profile` | a fallback client delivered the stream |
| `incomplete-fallback` | a client returned an incomplete stream; another was tried |
| `web-context-fallback` | the WEB player context failed; the configured chain took over |
| `web-context-retry` | the WEB player context was capped and retried once with a fresh one |
| `watch-page-no-token` | a WEB run fell back to the watch page, so its PO token was never used |
| `url-re-resolved` | an expired stream URL was re-resolved mid-transfer |
| `session-rotated` | a fresh guest session replaced one the server kept rejecting |
| `rate-limited-retried` | a request was retried after a 429 |
| `throttled` | a host rate-limited the run; requests to it are held for the stated wait |
| `playlist-entry-failed` | one playlist entry failed; the rest were returned |
| `metadata-embed-failed` | `--embed-thumbnail` or `--embed-metadata` could not write everything |
| `tag-carry-incomplete` | some of the input's metadata did not reach the output |
| `loudness-target-missed` | the loudness target was not reached; the detail says why |
| `loudness-unmeasurable` | an integrated loudness came back non-finite |
| `output-clipping` | the delivered file peaks past full scale |
| `implicit-downmix` | the encoder folded channels the request did not ask to lose |
| `implicit-lossy` | the output container could not hold the source codec, so a lossy encoder the request never named ran |
| `bitrate-adjusted` | the encoder used the nearest bit rate it supports |
| `cut-snapped` | a packet-level cut moved interior joins to the packet grid |
| `cut-decoded` | a `smart` cut had to decode a lossy source, a second generation |
| `gapless-dropped` | a copy into raw ADTS (`.aac`) kept the encoder delay and padding as audio |
| `input-damage` | the input was damaged; the readable audio was delivered, and `--json` counts what is missing as `inputLoss` |
| `input-note` | the decoder's remarks on an undamaged input |
| `empty-input` | the input's audio track holds no frames |
| `source-policy-unmatched` | `--source-policy prefer:<codec>` named a codec the video does not offer |

| Note | Meaning |
|---|---|
| `alac-mp4-container` | `.alac` output is an MP4 container; `.m4a` is the usual name |
| `archive-not-recorded` | a finished file was not added to the download archive; a re-run fetches it again |
| `channels-ignored` | `--itag` names an exact encoding, so `--channels` had no say |
| `channels-unavailable` | the requested layout was not available; the delivered one is named |
| `concurrency-clamped` | `--concurrency` exceeded the maximum and was clamped |
| `container-ext-mismatch` | the output extension does not match the source container, which was copied unchanged |
| `cover-art-remuxed` | the source was remuxed, packets unchanged, into a container that can hold cover art |
| `cue-data-track` | the CUE sheet's data track was skipped; the pieces keep the disc's numbering |
| `cue-file-mismatch` | the CUE sheet names a different file than the rip being split |
| `doctor-caveat` | a `doctor` check passed with a caveat; the detail says what it did not prove |
| `enumeration-error` | a playlist page failed to enumerate; the run continued |
| `flag-inert` | a flag had no effect on this run |
| `forced-client-risky` | a forced `--client` is known to deliver unreliably |
| `kept-output` | a finished file was kept after a failure elsewhere in the run |
| `length-unchecked` | the SponsorBlock preview could not fetch the video's length to check segments against |
| `playlist-ignored` | a playlist URL was passed to a video command; the video was used |
| `probe-skipped` | `--probe` read nothing: the selected stream is SABR-only |
| `same-format-copied` | the source is already the target format and was copied; `--force` re-encodes (`transcode --force` for a download) |
| `selection-unmatched` | no audio format matched the requested selection |
| `sidecar-write-failed` | the `--write-info-json` sidecar could not be written |
| `unaltered-copy` | the output is a byte-for-byte copy of the source |
| `watch-page-formats` | the format list came from the watch-page fallback, which needs no PO token |
| `web-sources` | only one WEB identity source is configured, where a second could have helped |
