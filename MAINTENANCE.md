# Maintaining WaxTap

YouTube's player, client profiles, and anti-bot behavior change without notice.
This runbook covers diagnosis, runtime recovery, fixtures, and releases.

## Breakage response

### 1. Confirm the failure

```sh
waxtap doctor                         # extract, resolve, and read a small range
waxtap doctor --full                  # also download a complete track
waxtap doctor --video <id-or-url>     # check a specific video
```

`doctor` tries several known-good videos so one removed video does not determine
the result.

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
GOOS=windows GOARCH=amd64 go build ./...
GOOS=darwin GOARCH=arm64 go build ./...
```

Live tests can be rate-limited or bot-walled from CI or datacenter IPs. A skip is
expected; a cipher failure is not.

CI runs formatting, vet, builds, race tests, and cross-compiles. The daily
`doctor` workflow fails only on exit 4; availability and rate-limit failures
remain warnings.

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
`--chrome-major` cannot be combined with `--profile-override`.

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
      "name": "ANDROID_VR",
      "innerTubeName": "ANDROID_VR",
      "innerTubeId": 28,
      "version": "1.65.10",
      "userAgent": "com.google.android.apps.youtube.vr.oculus/1.65.10 (Linux; U; Android 12L; eureka-user Build/SQ3A.220605.009.A1) gzip",
      "deviceMake": "Oculus",
      "deviceModel": "Quest 3",
      "osName": "Android",
      "osVersion": "12L",
      "androidSdkVersion": 32
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
| HTTP 4xx except 408/429 | 2 |
| HTTP 429 | 5 |
| Connection failure, HTTP 408/5xx, or invalid response | 9 |

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

If present, `playability_status` must be `OK`.
`player_url` is needed when the streaming URL's `n` parameter must be
descrambled. Format entries require enough identity to select and request the
audio, especially `itag`, `lmt`, `xtags`, and `mime_type`; richer quality,
duration, DRC, and track fields are optional.

`--player-context-url` requires `--potoken-url`, and the context mint and
download must share an egress IP because the signed URL is IP-bound.

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

The `/session` response contains the exact `visitor_data` literal and optional
cookies. The camelCase key `visitorData` is also accepted. Adoption requires a
single-client chain and drops login cookies. The session is resolved once per
`Client`, and adoption failures are fatal. The minter and download must share
an egress IP.

## SABR audio

ANDROID_VR uses direct signed URLs. WEB-family clients expose URL-less audio over
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

## Releasing

```sh
git tag vX.Y.Z
git push origin vX.Y.Z
```

The release workflow runs GoReleaser and creates a draft GitHub release. Use
`goreleaser release --snapshot --clean` for a local dry run or
`goreleaser check` for configuration validation.
