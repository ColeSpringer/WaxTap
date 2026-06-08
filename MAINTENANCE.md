# Maintaining WaxTap

WaxTap extracts audio from YouTube, whose player and anti-bot surfaces change
without notice. This document is the playbook for when something breaks, plus the
reference for the knobs that let you respond without rebuilding.

## Quick response: YouTube broke extraction

1. **Confirm it.** Run the health check:
   ```
   waxtap doctor            # extract + resolve + small byte read (cheap)
   waxtap doctor --full     # also download a whole track
   waxtap doctor --video-id <id-or-url>   # check a specific video
   ```
   `doctor` tries a list of known-good videos, so one deleted video does not
   decide the result. Exit code `4` means an extraction/cipher failure: the class
   that usually requires a WaxTap code or profile update. Exit `3` means
   availability/restriction, and exit `5` means rate limiting; both are often
   environmental, especially from datacenter IPs.

2. **Capture what YouTube returned.** Set the dump directory and reproduce:
   ```
   WAXTAP_DUMP_DIR=./dump waxtap info <url>
   ```
   On an extraction failure, WaxTap writes the raw player response(s) it could not
   use to `./dump/<timestamp>-playerresponse-<client>-<videoID>.json` (and the
   watch page on a watch-page parse failure). These are the ground truth for
   diagnosis. They are diagnostic only and never change WaxTap's behavior.

3. **Capture a fresh base.js** (the cipher source). WaxTap already persists it in
   the on-disk cache (see below); you can also fetch it directly. Find the current
   player URL from any watch/embed page (`/s/player/<hash>/.../base.js`) and:
   ```
   curl -s 'https://www.youtube.com/s/player/<hash>/player_ias.vflset/en_US/base.js' -o base.real.js
   ```
   `*.real.js` and `testdata/real/` are git-ignored on purpose (licensing): use
   them locally to derive an **authored, minimized** fixture, never commit the raw
   file.

4. **Fix the smallest surface.** Breakage usually lands in one of a few files:
   | Symptom | File |
   |---|---|
   | Bot wall / playability `ERROR` / stale client version | `internal/clientident` (WEB-family Chrome and InnerTube versions); `youtube/profile.go` (other client versions, device fingerprints, and PO-token requirements) |
   | Signature / `n`-parameter solve fails (exit 4, `ErrCipherSolve`) | `youtube/internal/resolver/solver.go` (whole-player parse/unwrap, descrambler fingerprint, consensus) + `env.js` (browser-global stub) |
   | WEB/WEB_EMBEDDED `/player` returns `UNPLAYABLE` while mobile clients work | `youtube/internal/resolver/cipher.go` (`stsPatterns`); discovery loads the regular `player_es6` build (watch-first + `bpctr`, `player.go:discoverPlayerURL`); see [SABR audio streaming](#sabr-audio-streaming) |
   | Player response shape changed (parse/format extraction) | `youtube/playerresponse.go` |
   | WEB audio stalls, truncates, or fails to decode | `youtube/internal/sabr` (UMP part ids, protobuf field numbers); see [SABR audio streaming](#sabr-audio-streaming) |

   Reproduce against your captured fixture, adjust, and run the checks below.
   If the recovery path or runtime knobs changed, update this file in the same
   patch.

   **How the cipher is solved.** The transforms are not carved out of base.js;
   the **whole player runs in goja** and its own descrambler does the work
   (`solver.go`). The flow: parse base.js once, AST-unwrap the player IIFE to
   global scope, fingerprint the descrambler by a direct `obj.method("alr","yes")`
   body statement, drive `n`/signature through the player's URL object, and accept
   a result only by consensus (every non-throwing candidate must agree on one
   value, else `ErrCipherSolve`). Running the whole player needs the browser-global
   stub in `env.js`. Failure modes:
   - a runtime `X is not defined` (shown in the `ErrCipherSolve` wrap and the
     resolver's warn logs) means a rotated player references a global the stub
     lacks - add one line to `env.js`'s explicit, fail-loud list;
   - sts=0 / `UNPLAYABLE` is usually discovery fetching a consent/HTML page, not a
     `stsPatterns` miss - the warn log prints the discovered URL, body length, and
     first bytes to tell the two apart;
   - a `parse`/`compile` error means goja cannot handle a construct in the current
     player - a goja ES-floor ceiling (`TestGojaES6Floor`), not a solver bug.

## Verifying a fix

- **Unit (offline), authored fixtures:** `go test ./...`
- **Race:** `go test -race ./...`
- **Live (network), build-tagged:** `go test -tags=integration ./...`
  Live tests hit real YouTube and may be rate-limited or bot-walled from
  datacenter/CI IPs; a skip there is expected, a cipher failure is not.
- **Cross-compile:** `GOOS=windows GOARCH=amd64 go build ./...` and
  `GOOS=darwin GOARCH=arm64 go build ./...`

CI (`.github/workflows/ci.yml`) runs gofmt, vet, build, race tests, and
cross-compile on every push/PR. The daily `doctor` cron
(`.github/workflows/doctor-cron.yml`) is an early-warning probe: it only fails the
job on an extraction/cipher failure (exit 4). Availability and rate-limit
failures warn and keep the job green because GitHub runner IPs are frequently
throttled by YouTube.

## Bumping the emulated Chrome version

Some player PO tokens are bound to the Chrome version in the browser identity. A
stale major can cause YouTube to reject the token at the `/player` endpoint,
leaving formats without stream URLs. The emulated version only needs to be
reasonably current; it does not need to match the latest release.

`internal/clientident` centralizes the built-in WEB-family identity: the Chrome
major (`DefaultChromeMajor`), the reduced desktop User-Agent, and the InnerTube
`WEB` and `WEB_EMBEDDED` versions. Update those values when rebuilding. To find
the current stable Chrome major without authentication, query
`versionhistory.googleapis.com/v1/chrome/platforms/win/channels/stable/versions`.

To update the emulated major without rebuilding, use a runtime override:

```sh
waxtap --chrome-major 151 info <url>
# or: WAXTAP_CHROME_MAJOR=151, or "chromeMajor": 151 in config.json
```

`--chrome-major` updates only the built-in WEB-family identities. These identities
are used by the default client chain, player discovery, visitor-data bootstrap,
watch-page fallback, and playlist fallback. The valid range is `0..999`; `0`
selects the built-in default. Values outside that range are rejected at startup.
The option cannot be combined with `--profile-override`, which supplies its own
user agents.

## Client-profile override files

When YouTube only needs a **client version or user-agent bump**, you do not need a
rebuild. Point WaxTap at a JSON file that replaces the built-in client strategy
chain:

```sh
waxtap --profile-override ./profiles.json info <url>
# or: WAXTAP_PROFILE_OVERRIDE=./profiles.json, or "profileOverridePath" in config.json
```

The file declares the full ordered chain; the first client that works wins. An
override replaces the built-ins, so include every fallback you still want. This
template mirrors the current defaults in `youtube/profile.go`:

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
      "androidSdkVersion": 32,
      "requiresPoTokens": [],
      "supportsPlaylists": false
    },
    {
      "name": "IOS",
      "innerTubeName": "IOS",
      "innerTubeId": 5,
      "version": "21.02.3",
      "userAgent": "com.google.ios.youtube/21.02.3 (iPhone16,2; U; CPU iOS 18_3_2 like Mac OS X;)",
      "deviceMake": "Apple",
      "deviceModel": "iPhone16,2",
      "osName": "iPhone",
      "osVersion": "18.3.2.22D82",
      "requiresPoTokens": ["gvs"],
      "supportsPlaylists": false
    },
    {
      "name": "WEB_EMBEDDED_PLAYER",
      "innerTubeName": "WEB_EMBEDDED_PLAYER",
      "innerTubeId": 56,
      "version": "1.20260115.01.00",
      "userAgent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
      "requiresPoTokens": [],
      "supportsPlaylists": false,
      "needsSignatureTimestamp": true,
      "embedUrl": "https://www.reddit.com/"
    },
    {
      "name": "WEB",
      "innerTubeName": "WEB",
      "innerTubeId": 1,
      "version": "2.20260603.05.00",
      "userAgent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
      "requiresPoTokens": ["player", "gvs"],
      "supportsPlaylists": true,
      "needsSignatureTimestamp": true
    }
  ]
}
```

Notes:

- `requiresPoTokens` is a list of scope names. WaxTap currently applies `player`
  and `gvs`; omit the field or use `[]` for none. The `none` sentinel must appear
  alone, and unsupported scopes such as `subtitles` are rejected.
- `needsSignatureTimestamp` must be `true` for WEB-family clients that decipher
  signatures (`WEB`, `WEB_EMBEDDED_PLAYER`); without it the `/player` request omits
  the timestamp and YouTube returns `UNPLAYABLE`, so a forced-WEB override never
  reaches SABR. Mobile clients on direct URLs (`ANDROID_VR`, `IOS`) leave it unset.
- `embedUrl` sets `context.thirdParty.embedUrl`, which `WEB_EMBEDDED_PLAYER`
  requires (a third-party embed origin, not youtube.com). Caveat: even with it,
  YouTube currently returns `This video is unavailable` (error 152) for the embedded
  client on many public videos - a selective/region restriction tracked upstream as
  yt-dlp #16077, not a WaxTap bug - so embedded is an unreliable fallback right now.
- Headers such as `X-Youtube-Client-Name` are derived from the scalar fields. Do
  not add a separate header map to the JSON.
- An override replaces only the primary extraction chain. Player discovery, the
  watch-page scrape, and playlist fallback still use the built-in WEB identity,
  so an override may use a different WEB User-Agent than those requests. The
  player, PO-token, and stream requests for an extraction still use its winning
  profile consistently, and discovery never requests a PO token. Use
  `--chrome-major` instead when only the built-in Chrome identity needs to change;
  the two options cannot be combined.
- The loader is strict: unknown keys, trailing data after the JSON document, an
  empty list, or a profile missing `name`/`innerTubeName`/`version`/a positive
  `innerTubeId` is a hard error. (`innerTubeId` drives `X-Youtube-Client-Name`;
  omitting it would derive `"0"`, which matches no real client.)

### Profile refresh checklist

When a client starts returning playability `ERROR`, HTTP 400, empty formats, or
URLs that now need a PO token, refresh the profile deliberately:

- Verify `clientName`, numeric `innerTubeId`, version, user agent, device fields,
  and whether the client should post to a different InnerTube host.
- Recheck `requiresPoTokens` against live behavior. Player-scope tokens go in the
  `/player` body during extraction, and GVS-scope tokens go on stream URLs during
  resolution. WEB declares `["player", "gvs"]`.
- Keep at least one playlist-capable profile (`WEB` today) in the chain unless
  playlist support has moved elsewhere.
- Add or update a fixture that captures the new behavior before changing parser
  or resolver code.

## On-disk cache

WaxTap persists YouTube's player JS (`base.js`, a few MiB that change only when
YouTube rotates the player) so a fresh process compiles the cipher from disk
instead of re-downloading it.

- Location: `waxtap cache dir` (default `os.UserCacheDir()/waxtap`, under
  `players/v<schema>/`). Override with `--cache-dir` / `WAXTAP_CACHE_DIR`.
- It is size-capped (LRU), schema-versioned, written `0600` with atomic
  temp+rename, and entirely best-effort: a read-only or full filesystem just
  degrades to network-only, it never fails extraction.
- Only genuine player JS is cached: a 200 response with no extractable cipher
  transform (a bot wall, captive portal, or truncated body) is never persisted,
  and an already-poisoned entry is ignored in favor of a fresh fetch. A
  transient bad response cannot wedge later runs.
- `waxtap cache clean` removes it; `--no-cache` / `WAXTAP_NO_CACHE` disables it.
- If you ever suspect a corrupt or stale player is cached, `cache clean` and
  re-run. WaxTap re-fetches whatever it needs.

## PO tokens

WaxTap does not ship a PO-token generator. It accepts a caller-supplied
`potoken.Provider` through `waxtap.Options.POTokenProvider`; if a profile requires
a token and no provider is configured, extraction or resolution fails with
`ErrNeedsPOToken`.

Two scopes are implemented:

- `player`: sent as `serviceIntegrityDimensions.poToken` in the `/player` request
  body during extraction.
- `gvs`: added to the googlevideo stream URL during resolution.

When integrating a provider:

- **Share the HTTP client and cookie jar.** Create a `*http.Client` with a jar and
  pass the same client to WaxTap (`Options.HTTPClient`) and the provider. That
  keeps the token minting request and the stream request on the same IP and
  browser/session identity. If you let WaxTap build its default client, its jar is
  internal to WaxTap and cannot be shared with an external provider.
- **Use the threaded user agent.** `potoken.Request.UserAgent` carries the exact
  UA WaxTap will send for the request that needs the token. Providers that bind
  tokens to request headers should mint with that UA.
- **Use the threaded failure.** On an expired or invalid stream token,
  `youtube.Client.ResolveWithFailure` passes the triggering
  `potoken.HTTPFailure` into the provider for diagnosis before re-resolving.

Cache by scope and binding. Player and GVS tokens are requested separately so a
provider can mint or reuse the right token for each scope.

## SABR audio streaming

The default client chain leads with ANDROID_VR, which returns direct signed
stream URLs, so ordinary downloads do not use SABR. The WEB and WEB_EMBEDDED
clients are different: their adaptive audio formats carry no `url` and no
`signatureCipher`, and are served only through YouTube's SABR protocol over the
UMP wire format (`streamingData.serverAbrStreamingUrl`). WaxTap reassembles those
segments into a single audio stream in `youtube/internal/sabr`. SABR activates
whenever a winning client returns URL-less audio. That is the WEB case, whether
WEB is forced (see [Client-profile override files](#client-profile-override-files))
or reached by fallback.

The WEB path has two requirements. Missing either one fails with a typed error
rather than dropping to low-quality audio; there is no legacy itag-18 fallback.

- A `signatureTimestamp` (sts) in the `/player` request. WaxTap reads it from
  `base.js` (`youtube/internal/resolver/cipher.go`, `stsPatterns`). A missing or
  stale sts makes `/player` return `UNPLAYABLE` before any formats are seen, so if
  WEB returns `UNPLAYABLE` while mobile clients work, suspect a zero sts before the
  PO token. The sts is read from the regular `player_es6` build, which player
  discovery loads watch-first (`player.go:discoverPlayerURL`); the embedded
  `player_embed_es6` build served from `/embed` returns sts=0 against `stsPatterns`,
  so the watch-first order is what keeps the value extractable.
- A GVS-scope `potoken.Provider`. The `gvs` token used on direct stream URLs is
  base64-decoded to raw bytes and carried in the SABR `streamerContext`.
  Attestation-required (`STREAM_PROTECTION_STATUS`) surfaces as `ErrNeedsPOToken`.

### When SABR breaks

The wire surface is volatile and lives in two files, both verified against a
pinned upstream revision recorded as `upstreamCommit` in `proto.go` (currently
`d2fa40d761034a286cf60ee033653307a1295b0c`, LuanRT/googlevideo, 2025-11-03).

- `youtube/internal/sabr/proto.go` holds the protobuf field numbers for the SABR
  request and response messages, hand-encoded with
  `google.golang.org/protobuf/encoding/protowire` (no generated code; CGO stays
  off). YouTube can rotate these numbers, and a stale one corrupts decoding
  silently instead of failing cleanly. When SABR decoding breaks after a protocol
  change, recheck the field numbers against that revision's `protos/` directory
  before changing parser logic, then bump `upstreamCommit` in the same patch.
  Decoders skip unknown fields, so additive changes stay compatible.
- `youtube/internal/sabr/ump.go` holds the UMP part ids and UMP's custom
  variable-length integer, which is not protobuf LEB128: the first byte's leading
  1-bits set the total length (1 to 5 bytes). The part-id constants run from
  `MEDIA_HEADER=20` to `STREAM_PROTECTION_STATUS=58` and come from the same
  revision (`protos/video_streaming/ump_part_id.proto`). Unknown part ids are
  skipped by their encoded size, so new parts stay compatible.

Two limitations matter if a specific video stalls or truncates:

- SABR is sequential (POST, consume segments, POST again until the format is
  complete), so it cannot use the parallel-chunk download path. A SABR download is
  single-stream.
- `SABR_CONTEXT_UPDATE` (57) and `SABR_CONTEXT_SENDING_POLICY` (59) are not
  implemented. That is fine for short audio, but it is the first thing to add if a
  video stalls before completing.

The CLI detects SABR without a provider, since routing to a SABR stream happens
before any token is minted: `info --show-urls` prints `SABR (no direct URL)` and
the JSON sets `resolved.isSabr`.

```sh
waxtap --profile-override ./profiles.json info <url> --show-urls
```

Reading the bytes (`download`, or `doctor`'s byte read) needs the GVS token. The
CLI ships no PO-token generator, so only a library consumer that supplies a
`potoken.Provider` can drive the WEB byte path (see [PO tokens](#po-tokens));
without one it fails with `ErrNeedsPOToken`.

## Fixtures policy

- **Committed:** authored / minimized fixtures under `youtube/testdata/` and
  `youtube/internal/resolver/testdata/` (a synthetic IIFE-wrapped `player_synth.js`
  exercising the whole-player solver, trimmed player-response JSON, etc.).
- **Never committed:** real YouTube `base.js` / player-response captures
  (licensing). `.gitignore` excludes `testdata/real/`, `*.real.js`, and
  `*.real.json`. Use real captures locally to author a minimal fixture, then
  delete them.

## Releasing

Release binaries (Linux/macOS/Windows, amd64/arm64) are built by GoReleaser.

```
git tag vX.Y.Z
git push origin vX.Y.Z      # triggers .github/workflows/release.yml
```

The workflow runs `goreleaser release --clean` and creates a **draft** GitHub
release for final review before publishing. For a local dry run, use
`goreleaser release --snapshot --clean`; for a config-only check, use
`goreleaser check`. After publishing,
`go install github.com/colespringer/waxtap/cmd/waxtap@vX.Y.Z` should also work
because the version subcommand reads module build info when no ldflags are
injected.

### API stability

The project may ship a breaking change in a minor release instead of moving to a
`/v2` module path when the affected API has no known consumers, as it did for
v1.1.0. Document these changes in the GitHub release notes.

- **v1.4.0** added SABR/UMP audio streaming for the WEB and WEB_EMBEDDED clients
  (`youtube/internal/sabr`) and the `signatureTimestamp` the WEB `/player` request
  needs. `youtube.Client.Resolve` and `ResolveWithFailure` now return
  `(MediaPlan, error)` instead of `(*ResolvedStream, error)`, since a SABR stream
  has no direct URL; the `waxtap` facade is unchanged, with `Client.Resolve` still
  returning a `ResolvedStream` via `MediaPlan.Diagnostic()`. The breaking change is
  confined to the exported-but-volatile `youtube` package. Profile override files
  gain a `needsSignatureTimestamp` key for WEB-family clients, and the release adds
  `google.golang.org/protobuf` (used via `encoding/protowire` only; CGO stays off).
- **v1.3.0** removed the unused `Politeness.MaxDownloadsPerRun` field. Per-run
  limits now use `PlaylistDownloadOptions.MaxDownloads`, which is enforced by
  `Client.DownloadPlaylist` and exposed by the download command's
  `--max-downloads` flag. This release also added `Politeness.Cooldown` and its
  `--cooldown`, `WAXTAP_COOLDOWN`, and `cooldownSeconds` settings.
