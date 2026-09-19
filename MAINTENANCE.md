# Maintaining WaxTap

YouTube's player, client profiles, and anti-bot behavior change without notice.
This runbook covers diagnosis, runtime recovery, fixtures, and releases. Work
cut from a change is tracked in [docs/deferred-work.md](docs/deferred-work.md)
and asks of the sibling Wax repos in
[docs/upstream-requests.md](docs/upstream-requests.md); add to both in the
same change that defers the work.

## Breakage response

### 1. Confirm the failure

```sh
waxtap doctor                         # extract, resolve, and read a small range
waxtap doctor --full                  # also download a complete track
waxtap doctor --video <id-or-url>     # check a specific video
```

`doctor` tries several known-good videos so one removed video does not determine
the result. `--full` reorders that list to lead with a ~10-minute track, since a
short clip proves the pipeline runs but not that a full-length delivery works;
it warns when the track it did deliver came in under 2 MiB, and `--json` records
every candidate that failed before one passed.

| Exit | Interpretation |
|---|---|
| 3 | unavailable/restricted content; usually not a maintenance issue |
| 4 | extraction, cipher, or playlist parsing failure; likely a maintenance issue |
| 5 | rate limiting; often environmental |
| 7 | incomplete delivery or expired URL; try another client |
| 8 | missing or rejected PO token |
| 9 | network, proxy, or sidecar failure |
| 10 | local I/O failure |

Forced iOS delivery is best-effort: a small range can pass while a longer
download later fails with exit 7. Full WEB delivery requires a GVS token plus
an attested `/player-context` or adopted `/session`.

### 2. Capture artifacts

```sh
WAXTAP_DUMP_DIR=./dump waxtap info <url>
```

On extraction failure, WaxTap writes unusable player responses and failed watch
pages to the dump directory. Dumps never change behavior.

For cipher work, capture the current player locally:

```sh
curl -s 'https://www.youtube.com/s/player/<hash>/player_ias.vflset/en_US/base.js' \
  -o base.real.js
```

Raw YouTube artifacts are git-ignored for licensing reasons. Use them to create
an authored, minimized fixture; never commit them.

### 3. Fix the smallest surface

| Symptom | Start here |
|---|---|
| Bot wall, stale client, playability `ERROR` | `internal/clientident`, `youtube/profile.go` |
| Signature or `n` solve failure | `youtube/internal/resolver/solver.go`, `env.js` |
| WEB `/player` is `UNPLAYABLE` while mobile works | `youtube/internal/resolver/cipher.go`, player discovery |
| Player response shape changed | `youtube/playerresponse.go` |
| WEB audio stalls, truncates, or misdecodes | `youtube/internal/sabr` |

The cipher solver executes the whole player in goja, fingerprints descrambler
candidates, and requires consensus. Common failures:

- `X is not defined`: add the missing explicit browser global to `env.js`.
- `sts=0` or `UNPLAYABLE`: inspect the discovered player URL and response before
  changing `stsPatterns`.
- Parse/compile failure: check whether the current player uses JavaScript syntax
  unsupported by goja.

Update this runbook when a recovery path or runtime control changes.

## Verification

```sh
go test ./...
go test -race ./...
go test -tags=integration ./...
go mod tidy -diff
goreleaser release --snapshot --clean
```

Live tests can be rate-limited or bot-walled from CI or datacenter IPs. A skip is
expected; a cipher failure is not.

On Windows the suite can fail across every httptest-backed package at once with
`connectex: Only one usage of each socket address`. That is ephemeral-port
exhaustion, not a code failure: the default pool is 16384 ports and each is held
for 120s after close. A full run costs about 135, so this only bites when
something else on the box is already consuming the pool, and the retry tests in
`internal/httpx` and `download` amplify it once dials start failing. Confirm with
`netstat -an | grep -c TIME_WAIT`, work around it with `go test -p 1 ./...`, and
remove the ceiling with `netsh int ipv4 set dynamicport tcp start=16384
num=49151` (admin). Linux and macOS CI are unaffected, and the Windows leg
starts on a fresh runner with an empty pool.

CI runs gofmt, vet, and a `go mod tidy` check, the race tests on Linux, macOS,
and Windows (shuffled), and a GoReleaser snapshot of every release target.
`vulncheck` runs govulncheck against each release target on every change and
weekly. GitHub switches a schedule off after 60 days without a commit and emails
the owner; re-enable it from the Actions tab.

Live extraction health is not checked from CI. A GitHub-hosted address is
bot-walled in the player response, before the cipher path runs, so a scheduled
`doctor` could never observe the exit-4 alarm it existed for: all 105 scheduled
runs from 2026-06-04 to 2026-09-17 refused with `login-required` on every
candidate and none reached YouTube. Run `waxtap doctor` from a served address
before a release.

## Client identity

### Chrome identity

`internal/clientident` owns the built-in WEB-family Chrome major, reduced
User-Agent, and InnerTube versions. Keep them reasonably current and update them
together when rebuilding. Chrome stable versions are available from
`versionhistory.googleapis.com`.

To test a Chrome major without rebuilding:

```sh
waxtap info <url> --chrome-major 151
# Also: WAXTAP_CHROME_MAJOR=151 or {"chromeMajor":151}
```

The valid range is `0..999`; `0` selects the built-in default.
`--chrome-major` cannot be combined with `--profile-override`. An adopted
session that carries its own `user_agent` outranks both on the adopted chain,
including a WEB profile loaded from `--profile-override`: the cookies, the
visitor id, and the PO token all belong to that browser, so the requests
carrying them present it too.

### Profile overrides

`--profile-override` replaces the complete ordered client chain, allowing
client-version, User-Agent, or device-identity updates without rebuilding:

```sh
waxtap info <url> --profile-override ./profiles.json
# Also: WAXTAP_PROFILE_OVERRIDE=./profiles.json
```

Use the current defaults in `youtube/profile.go` as the template. A minimal
single-client file looks like:

```json
{
  "profiles": [
    {
      "name": "VISIONOS",
      "innerTubeName": "VISIONOS",
      "innerTubeId": 101,
      "version": "1.02",
      "userAgent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_7_3) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15",
      "deviceMake": "Apple",
      "deviceModel": "RealityDevice17,1",
      "osName": "visionOS",
      "osVersion": "26.5.23O471"
    }
  ]
}
```

Required fields are `name`, `innerTubeName`, `innerTubeId`, and `version`.
The loader rejects unknown fields, trailing data, empty chains, and unsupported
PO-token scopes.

When refreshing profiles:

- Verify the name, numeric ID, version, User-Agent, device fields, and host.
- Recheck `requiresPoTokens`; supported scopes are `player` and `gvs`.
- Set `needsSignatureTimestamp` for WEB-family profiles.
- Set a third-party `embedUrl` for `WEB_EMBEDDED_PLAYER`.
- Keep a playlist-capable profile if playlist support is required.
- Add or update a minimized fixture before changing parser logic.

An override affects only the primary extraction chain. Discovery, watch-page
scraping, and playlist fallback still use the built-in WEB identity.

## Player-JS cache

WaxTap persists `base.js` under `waxtap cache dir` so new processes can reuse a
compiled player.

- The cache is size-capped, schema-versioned, atomically written, and
  best-effort. Filesystem failures fall back to the network.
- Responses without a usable cipher transform are not cached.
- Use `waxtap cache clean` when corruption is suspected.
- Use `--no-cache` or `WAXTAP_NO_CACHE` to disable it.

## PO tokens and sidecars

WaxTap does not ship a PO-token generator. Library users supply
`Options.POTokenProvider`; the CLI uses a bgutil-compatible `--potoken-url`.

Supported scopes:

- `player`: sent in the `/player` request, bound to the video ID.
- `gvs`: sent for stream delivery, bound to session `visitorData`.

Library providers should share WaxTap's HTTP client and cookie jar, mint with
`potoken.Request.UserAgent`, and cache by scope and binding. On stream-token
failure, `ResolveWithFailure` passes the triggering HTTP failure back to the
provider.

### CLI sidecars

`--potoken-url`, `--player-context-url`, and `--session-url` accept a base URL
or full endpoint. They preserve query parameters, bypass `--proxy`, and do not
follow redirects. Use HTTPS remotely. `--api-key` sends `X-API-Key` to every
configured sidecar.

Sidecar response classification:

| Failure | Exit |
|---|---|
| HTTP 422 `video-unavailable` | 3 (the playability verdict's class) |
| HTTP 4xx except 408/422/429 | 2 |
| HTTP 429 | 5 |
| Connection failure, HTTP 408/5xx, or invalid response | 9 |

A refusal is retried once. A transport failure or an HTTP 408/5xx earns it after
the wait the sidecar stated (`Retry-After` or `retry_after_seconds`), else after
500 ms; a 429 earns it only with a stated wait, since a bare 429 says back off.
Any other refusal, including a playability verdict, does not. A stated wait over
60 s is reported rather than slept through. The decision is exported as
`waxtap.SidecarRetryWait`, so an in-process adapter translating another client's
failures into `SidecarError`/`SidecarResponseError` runs WaxTap's rule instead of
a second one that drifts from it.

Every sidecar request is bounded by `WithSidecarTimeout` (CLI
`sidecarTimeoutSeconds`), 60 s by default; a `/session` resolution runs under
`Timeouts.WebContext` like a `/player-context` call, ahead of the extraction
budget.

A bot check the sidecar's browser hits ("Sign in to confirm you're not a bot")
buys a fresh identity once every 10 minutes; past that WaxSeal refuses the video
as `player-context-failed` (HTTP 502) with a 2 minute `Retry-After`. That wait is
past the 60 s cap, so WaxTap reports it rather than sleeping through it: the
context arm is parked for the stated wait (capped at 5 minutes) and the native
chain serves the rest of the batch meanwhile. With nothing delivering, it is exit
9 with the wait in the hint.

Error precedence across a download's attempts: `waxerr.PreferErr` ranks
availability verdicts above everything else, but an attempt that reached the
transfer proves the video is there (a `/player` response carried playable
formats and a signed URL resolved). Once one attempt has, a verdict from an
attempt that never got that far is dropped from the choice, so a bot-checked
sidecar or a token-less WEB client cannot make a video that demonstrably
streams report exit 3. A verdict hit by the attempt that was itself delivering
still stands, since that is the video refusing mid-download. Every dropped
cause stays in the per-attempt detail `IncompleteDeliveryError.Attempts`
renders.

Player-context failures and GVS-token failures detected before delivery may
fall back to the configured client chain. After delivery starts, token-refresh
failures are terminal.

### Player-context contract

The CLI posts `{"video_id":"..."}` to `/player-context`. A usable snake_case
response must include:

- `server_abr_streaming_url`
- `visitor_data`
- `video_playback_ustreamer_config`
- non-empty `audio_formats`

If present, `playability_status` must be `OK`. A non-200 answer carries
`{"error","code"}` and, for `video-unavailable`, `details` holding the
playability status; the status is classified as a `/player` status would be.
`player_url` is needed when the streaming URL's `n` parameter must be
descrambled. Format entries require enough identity to select and request the
audio, especially `itag`, `lmt`, `xtags`, and `mime_type`; richer quality,
duration, DRC, and track fields are optional. An optional `session_generation`
names the daemon session behind the context.

Optional identity keys: `user_agent` and `client_version`, the exact
`navigator.userAgent` and InnerTube client version the context was minted under.
When set, the WEB requests under the context (the SABR stream and the GVS token
request) carry that browser's identity in place of WaxTap's own, so
`--chrome-major` does not apply there; a context that states neither leaves them
to WaxTap's own WEB identity. This is the `/session` contract's pair, on the
per-video call.

Optional metadata keys: `channel_id`, `description`, `thumbnails` (`url`,
`width`, `height`, in the player response's order), `is_live_content`,
`is_live_now`, `is_upcoming`, and `publish_date` (RFC 3339 or `2006-01-02`).
They fill the delivered `Video` as a `/player` response would; a live or
upcoming flag refuses the context with the same sentinels.

`--player-context-url` requires `--potoken-url`, and the context mint and
download must share an egress IP because the signed URL is IP-bound.

When an attested stream caps and the once-retried fresh context caps too, the
session itself is suspect: WaxTap POSTs
`{"session_generation","video_id","reason":"stream-capped"}` to the `/report`
sibling of the player-context endpoint, then continues down the fallback chain.
The report retires the daemon session so the next download's context comes from
a fresh one. The `/report` response contract is the one in the session-adoption
section: 200 with a JSON object, and `retry_after_seconds` refuses the report.
A context without `session_generation` is never reported; an endpoint without
`/report` fails each report (404) and the session stays.

### Session adoption

Session adoption lets WEB delivery use the exact guest identity attested by a
token minter. The CLI accepts either:

```sh
waxtap download <url> --client web \
  --session-url http://127.0.0.1:4417/session \
  --potoken-url http://127.0.0.1:4417

waxtap download <url> --client web \
  --visitor-data 'Cgt...%3D%3D' --cookies ./cookies.txt \
  --potoken-url http://127.0.0.1:4417
```

The `/session` response contains the exact `visitor_data` literal, optional
cookies, an optional `session_generation` naming the session, and optional
`user_agent` and `client_version` (the attesting browser's identity, applied to
WEB requests under the session; WEB_EMBEDDED_PLAYER takes only the user agent).
`cookie_header` and `same_site` are ignored: the cookies array carries the same
information. The camelCase keys `visitorData`, `sessionGeneration`, `userAgent`,
and `clientVersion` are also accepted. Adoption requires
a single-client chain and drops login cookies. Adoption failures are fatal. The
minter and download must share an egress IP.

When googlevideo caps delivery on the adopted session (empty-body 403 past
roughly 1 MB, well before the URL expires), WaxTap POSTs
`{"session_generation","video_id","reason"}` to the `/report` sibling of the
session endpoint, then re-resolves `/session` for the replacement. `/report`
must answer 200 with a JSON object; an empty body, a 204, or a response
carrying `retry_after_seconds` counts as a refusal and the session is kept. A
minter that omits `session_generation` is never reported; one without `/report`
fails each report (404). Either way a capped session cannot be rotated and the
download fails.

## SABR audio

VISIONOS, ANDROID_VR, and IOS use direct signed URLs. WEB-family clients expose URL-less audio over
SABR/UMP, implemented in `youtube/internal/sabr`. SABR is sequential and cannot
use the parallel chunk downloader.

Important invariants:

- WEB `/player` needs a signature timestamp from the regular player build.
- SABR audio selection uses `selected_audio_format_ids` so the server sends a
  WebM initialization segment.
- `STREAM_PROTECTION_STATUS=2` is pending and may deliver a roughly one-minute
  preview; status `3` is terminal and maps to `ErrNeedsPOToken`.
- A complete WEB stream needs an attested status-1 player context or a coherent
  adopted session. A GVS token alone does not lift the preview cap.
- Reassembly must write the initialization segment first and never return a
  headerless partial file as complete.

### Protocol changes

`youtube/internal/sabr/proto.go` contains hand-encoded protobuf field numbers
and records the pinned LuanRT/googlevideo revision as `upstreamCommit`.
`youtube/internal/sabr/ump.go` contains UMP part IDs and its custom varint.

When decoding breaks:

1. Compare field numbers and part IDs with the pinned upstream revision.
2. Verify UMP varint byte order against the upstream reader and writer code and
   literal wire-vector tests.
3. Bump `upstreamCommit` with any protocol update.
4. Prefer skipping unknown fields/parts over rejecting additive changes.

### Stall diagnosis

A status-2 stream that delivers about one minute and then stops returning media
has reached the known attestation-pending preview cap. Do not treat it as a
token or request-shape problem; use an attested player-context or adopted
session.

For other stalls:

```sh
WAXTAP_SABR_DUMP_DIR=/tmp/sabr waxtap download -v --client web \
  --potoken-url http://127.0.0.1:4416 \
  --session-url http://127.0.0.1:4416/session \
  --dir /tmp/out "https://www.youtube.com/watch?v=VIDEO_ID"

WAXTAP_SABR_DUMP_DECODE=/tmp/sabr go test -tags=integration \
  -run TestDecodeSABRDumps ./youtube/internal/sabr/ -v
```

Inspect `player_time_ms`, effective duration/range values, media-part counts,
repeated sequence numbers, and mid-stream format changes. Dumps are best-effort
and do not alter streaming.

## Fixtures

Commit only authored, minimized fixtures under `youtube/testdata/` and
`youtube/internal/resolver/testdata/`. Never commit real `base.js` or player
responses. `.gitignore` excludes `testdata/real/`, `*.real.js`, and
`*.real.json`.

Audio fixtures are synthesized by the engine at test time (`internal/mediatest`
writes WAV, the tests encode from it). The exceptions are the formats WaxFlow
only decodes, which nothing here can write: `internal/mediatest/testdata/`
holds WaxFlow's own synthetic fixtures for them. `tagged.mpc` and `chapters.mpc`
are reference-encoder renders of a generated seed, the second with chapters
the reference chapter editor wrote; `chapters.wma` is an ffmpeg render of a
generated sine with a chapter list; `lossless.wma` is a synthesized signal
written by Windows' own WMA Lossless encoder; `alaw.wav` is an ffmpeg G.711
render of a generated sine, and `mp3.wav` the frames of a generated MP3
wrapped in a WAV by ffmpeg's muxer, the two codec-in-a-writable-container
cases. Nothing under `testdata/` is a real recording.

## Releasing

```sh
git tag vX.Y.Z
git push origin vX.Y.Z
```

The release workflow runs GoReleaser, creates a draft GitHub release, and
attests the archives; for a release cut after 2026-09-17, `gh attestation
verify <archive> --repo ColeSpringer/WaxTap` ties a download to the workflow
run and the tagged commit. A re-run for the same tag fills the existing draft
and replaces its assets, keeping the notes. CI runs the same GoReleaser
configuration as a snapshot on every push. Use `goreleaser release --snapshot
--clean` for a local dry run or `goreleaser check` for configuration
validation.

Every archive carries `THIRD-PARTY-NOTICES.md` beside `LICENSE`: the license
and notice files of each module compiled into the binary, at the module root
and beside each linked package, WaxFlow's ported-code attributions included.
It is generated from the module cache, so regenerate it after any dependency
change:

```sh
go run ./internal/notices/gen
```

`TestCheckedInFileIsCurrent` in `internal/notices` fails while the file is
stale. It reads `go.mod` and `go.sum`, so a bump invalidates its cached result;
CI runs it, and the GoReleaser before-hook runs it again, so a stale file fails
the release instead of shipping under the tag. The generator runs `go list` with
`GOWORK=off`: a local workspace over the Wax repos does not change what the
release build links.
