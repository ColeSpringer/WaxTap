# Maintaining WaxTap

WaxTap extracts audio from YouTube, whose player and anti-bot surfaces change
without notice. Use this guide to diagnose extraction failures, update client
profiles, and verify fixes.

## Quick response: YouTube broke extraction

1. **Confirm it.** Run the health check:
   ```
   waxtap doctor            # extract + resolve + small byte read (cheap)
   waxtap doctor --full     # also download a whole track
   waxtap doctor --video <id-or-url>      # check a specific video
   ```
   `doctor` tries a list of known-good videos, so one deleted video does not
   decide the result. Exit code `4` means an extraction/cipher failure: the class
   that usually requires a WaxTap code or profile update. Exit `3` means
   availability/restriction, and exit `5` means rate limiting; both are often
   environmental, especially from datacenter IPs. Exit `7` (`incomplete-stream`)
   means WaxTap could not obtain a complete stream from the attempted clients,
   for example because of a WEB SABR preview limit. This is not a WaxTap
   maintenance signal, so it remains distinct from exit `4`. Exit `9` is a
   network/connection failure (dead `--proxy` or unreachable sidecar) and exit
   `10` is a local I/O failure; neither is a maintenance signal. Full WEB audio
   needs the GVS token (`--potoken-url`) **plus** an attested identity, either
   `--player-context-url` or `--session-url`. `--client ios` is metadata/formats
   only and rejects downloads before extraction with exit `2`
   (`delivery-unsupported`).

2. **Capture what YouTube returned.** Set the dump directory and reproduce:
   ```
   WAXTAP_DUMP_DIR=./dump waxtap info <url>
   ```
   On an extraction failure, WaxTap writes the raw player response(s) it could not
   use to `./dump/<timestamp>-playerresponse-<client>-<videoID>.json` (and the
   watch page on a watch-page parse failure). Keep these files for diagnosis.
   Dumping is diagnostic only and never changes WaxTap's behavior.

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
waxtap info <url> --chrome-major 151
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
waxtap info <url> --profile-override ./profiles.json
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

- `requiresPoTokens` is a list of scope names. WaxTap supports `player` and
  `gvs`; omit the field or use `[]` for none. The `none` sentinel must appear
  alone, and unsupported scopes such as `subtitles` are rejected.
- `needsSignatureTimestamp` must be `true` for WEB-family clients that decipher
  signatures (`WEB`, `WEB_EMBEDDED_PLAYER`); without it the `/player` request omits
  the timestamp and YouTube returns `UNPLAYABLE`, so a forced-WEB override never
  reaches SABR. Mobile clients on direct URLs (`ANDROID_VR`, `IOS`) leave it unset.
- `embedUrl` sets `context.thirdParty.embedUrl`, which `WEB_EMBEDDED_PLAYER`
  requires (a third-party embed origin, not youtube.com). Caveat: even with it,
  YouTube returns `This video is unavailable` (error 152) for the embedded client
  on many public videos. That is a selective/region restriction tracked upstream
  as yt-dlp #16077, not a WaxTap bug, so embedded is an unreliable fallback.
- Headers such as `X-Youtube-Client-Name` are derived from the scalar fields. Do
  not add a separate header map to the JSON.
- An override replaces only the primary extraction chain. Player discovery, the
  watch-page scrape, and playlist fallback use the built-in WEB identity,
  so an override may use a different WEB User-Agent than those requests. The
  player, PO-token, and stream requests for an extraction use its winning
  profile consistently, and discovery never requests a PO token. Use
  `--chrome-major` instead when only the built-in Chrome identity needs to change;
  the two options cannot be combined.
- The loader is strict: unknown keys, trailing data after the JSON document, an
  empty list, or a profile missing `name`/`innerTubeName`/`version`/a positive
  `innerTubeId` is a hard error. (`innerTubeId` drives `X-Youtube-Client-Name`;
  omitting it would derive `"0"`, which matches no real client.)

### Profile refresh checklist

When a client starts returning playability `ERROR`, HTTP 400, empty formats, or
URLs that require a PO token, refresh the profile deliberately:

- Verify `clientName`, numeric `innerTubeId`, version, user agent, device fields,
  and whether the client should post to a different InnerTube host.
- Recheck `requiresPoTokens` against live behavior. Player-scope tokens go in the
  `/player` body during extraction, and GVS-scope tokens go on stream URLs during
  resolution. WEB declares `["player", "gvs"]`.
- Keep at least one playlist-capable profile (`WEB` today) in the chain unless
  playlist support has moved elsewhere.
- Add or update a fixture that captures the response before changing parser or
  resolver code.

## On-disk cache

WaxTap persists YouTube's player JS (`base.js`, a few MiB that change only when
YouTube rotates the player) so a fresh process compiles the cipher from disk
instead of re-downloading it.

- Location: `waxtap cache dir` (default `os.UserCacheDir()/waxtap`, under
  `players/v<schema>/`). Override with `--cache-dir` / `WAXTAP_CACHE_DIR`.
- It is size-capped (LRU), schema-versioned, written `0600` with atomic
  temp+rename, and best-effort. A read-only or full filesystem falls back to
  network fetches and does not fail extraction.
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

### Authenticating sidecars

Use `--api-key`, `WAXTAP_API_KEY`, or `"apiKey"` in `config.json` when a
PO-token, player-context, or session sidecar requires API-key authentication.
WaxTap sends the value in the `X-API-Key` header to every configured sidecar. A
`401` or `403` response is reported as `invalid-config` (exit 2) with guidance to
set or verify the key. If a fallback client completes the request, the downgrade
warning includes the same guidance.

Sidecar clients do not follow redirects (`CheckRedirect` returns
`ErrUseLastResponse`), which prevents the authentication header from being
forwarded to another origin. Each sidecar setting accepts either a base URL or a
full endpoint URL, and existing query parameters are preserved by
`buildSidecarURL`. Use HTTPS for remote sidecars. Sidecar traffic bypasses
`--proxy`.

### Sidecar fallback behavior

Sidecar failures do not all trigger the same fallback behavior. If the
player-context request fails, WaxTap may continue with the configured client
chain. A successful fallback emits a web-context downgrade warning. If
player-context succeeds but the subsequent GVS token request (`get_pot`) fails,
WaxTap returns the token error (exit 9) instead of falling back to ANDROID_VR.
The request is already committed to the WEB context, and the error identifies
the unavailable endpoint.

### External session adoption

For byte-exact session coherence with a token minter, WaxTap can adopt an external
guest identity instead of bootstrapping its own. The `gvs` token's `content_binding`
is the session `visitorData`, so when a minter attests in a real browser, WaxTap can
stream under that browser's exact `visitorData`.

- **Inputs.** Library: `Options.Session` (a static `potoken.Session{VisitorData,
  Cookies}`) or `Options.SessionProvider` (pull-based, resolved once per `Client`).
  CLI: `--visitor-data` (+ optional `--cookies`, a Netscape file) for a static
  session, or `--session-url` for a provider that GETs
  `{"visitorData","cookies":[...]}`. `--session-url` is contacted directly, never
  via `--proxy`.
- **`visitorData` is sent verbatim.** It must be the browser's exact
  `X-Goog-Visitor-Id` literal (the URL-escaped `...%3D%3D` form `ytcfg.VISITOR_DATA`
  uses); WaxTap applies no escape/unescape, so it stays the same value the minter
  attests under, in the header, the InnerTube context, and the GVS `content_binding`.
- **Uniform chain required.** Adoption needs a single-client chain (`Options.Client`
  / `--client`, or a single-family `ProfileOverridePath`). The default multi-client
  chain is rejected so an adopted session is never routed through a client it was
  not minted for. `Client` and `ProfileOverridePath` are mutually exclusive, as are
  `Session` and `SessionProvider`.
- **Fatal on failure.** Under adoption a failed resolution aborts extraction rather
  than falling back to a synthetic `visitorData` (which would send the wrong
  `content_binding`). The session resolves **once per `Client`**, so long-running
  services should recreate the `Client` per task to pick up a fresh session.
- **Guest-only.** Login cookies (`SID`, `__Secure-3PSID`, `SAPISID`, and siblings)
  are dropped with a warning; a logged-in `visitorData` is account-bound. Supplying
  cookies without a jar is an error, not a silent drop; `visitorData`-only adoption
  is jarless.
- **Same egress IP.** The minter host and the WaxTap downloads must share an egress
  IP (use `--proxy` to align them if the minter runs elsewhere).
- **Two-pass ANDROID_VR then WEB.** Adoption forces a uniform chain, so there is no
  single-pass "default chain but adopt a session for the WEB fallback". Run the
  default chain first; only if it fails, run a second `Client` with `--client web
  --session-url ...` (or `--visitor-data ...`).

**WEB streams end to end with the full setup, but WEB is not the default path.**
A uniform WEB chain, an adopted coherent session, and a GVS provider on the
same egress IP download a complete, playable Opus/WebM (ffprobe-verified). The
pieces:

- **`selected_audio_format_ids` (field 16), not `selected_format_ids` (field 2).**
  Selecting audio through field 2 does not produce a WebM init. The server emits it
  (`FORMAT_INITIALIZATION_METADATA` + a `MEDIA_HEADER{is_init=1, seq=0}` whose bytes
  begin with the EBML magic) when the audio format is selected via field 16. This is
  what `buildRequest` sends; confirmed by diffing a browser SABR request.
- **`STREAM_PROTECTION_STATUS = 2` (PENDING) is non-terminal.** The googlevideo
  reference aborts only on `3` (REQUIRED). Status 2 streams media, so `consume`
  consumes it and continues; only status 3 yields
  `ErrNeedsPOToken`. A status-2 stream that ends with no end-segment or content
  length errors via `stallResult`, so a withheld partial is never served as
  complete.
- **Coherent session.** A working WEB run needs both `visitorData` and cookies from
  the same browser that mints the token, on the same egress IP. `--session-url`
  (a `/session` endpoint returning both) or `--visitor-data` + `--cookies` supplies
  it; `visitorData` alone is not enough. The reassembler (`stream.go:drain`) writes
  the `is_init` segment first, self-initializes only media that leads with the EBML
  magic, and never emits a headerless file.

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
rather than dropping to low-quality audio. WaxTap does not fall back to itag 18.

- A `signatureTimestamp` (sts) in the `/player` request. WaxTap reads it from
  `base.js` (`youtube/internal/resolver/cipher.go`, `stsPatterns`). A missing or
  stale sts makes `/player` return `UNPLAYABLE` before any formats are seen, so if
  WEB returns `UNPLAYABLE` while mobile clients work, suspect a zero sts before the
  PO token. The sts is read from the regular `player_es6` build, which player
  discovery loads watch-first (`player.go:discoverPlayerURL`); the embedded
  `player_embed_es6` build served from `/embed` returns sts=0 against `stsPatterns`,
  so the watch-first order is what keeps the value extractable.
- A GVS-scope `potoken.Provider`. The `gvs` token used on direct stream URLs is
  base64-decoded to raw bytes and carried in the SABR `streamerContext`. Only
  `STREAM_PROTECTION_STATUS = 3` (REQUIRED) surfaces as `ErrNeedsPOToken`; status 2
  (PENDING) is consumed and streamed (see
  [External session adoption](#external-session-adoption) for the full working
  setup). Status 2 caps WEB SABR at a ~1-minute preview for automated
  clients, and a better token does not lift it (see [Diagnosing a SABR
  stall](#diagnosing-a-sabr-stall)); full WEB audio comes from an attested
  identity, either a `/player-context` handoff (status-1 URL) or `/session`
  adoption. ANDROID_VR (which does not use WEB SABR) is the
  zero-dependency default.

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
  1-bits set the total length (1 to 5 bytes), and for the 1-to-4 byte forms the
  prefix's trailing `(8-size)` low bits hold the value's low bits with each
  following byte stacked above them (the inverse is `umpVarint` in the tests). This
  byte order is easy to invert, so verify it against LuanRT/googlevideo
  `src/core/UmpReader.ts` and `UmpWriter.ts` at the pinned `upstreamCommit`;
  `ump_test.go`'s wire-vector cases assert literal bytes (e.g. `32769` is
  `c1 00 04`), so an inversion fails fast. A byte-order error can decode a 32 KB
  `MEDIA` size as 66560 and mis-frame the rest of the stream, which resembles a
  withheld-media attestation problem. The part-id
  constants run from `MEDIA_HEADER=20` to
  `SABR_CONTEXT_SENDING_POLICY=59` and come from the same revision
  (`protos/video_streaming/ump_part_id.proto`). Unknown part ids are skipped by
  their encoded size, so unrecognized parts do not break decoding.

One limitation matters if a specific video stalls or truncates:

- SABR is sequential (POST, consume segments, POST again until the format is
  complete), so it cannot use the parallel-chunk download path. A SABR download is
  single-stream.

`SABR_CONTEXT_UPDATE` (57) and `SABR_CONTEXT_SENDING_POLICY` (59) are handled by
`applyContextUpdates` and `applyContextPolicy`, which fold updates into the stored
context and echo active types in subsequent requests. They are exercised only
after attestation passes (status 1), so offline tests cover them because the live
WEB path stops at the GVS gate described below.

The CLI detects SABR without a provider, since routing to a SABR stream happens
before any token is minted: `info --show-url` prints `SABR (no direct URL)` and
the JSON sets `resolved.isSabr`.

```sh
waxtap info <url> --profile-override ./profiles.json --show-url
```

Reading the bytes (`download`, or `doctor`'s byte read) needs the GVS token. The
CLI ships no PO-token generator, so only a library consumer that supplies a
`potoken.Provider` can drive the WEB byte path (see [PO tokens](#po-tokens));
without one it fails with `ErrNeedsPOToken`.

### Diagnosing a SABR stall

The known WEB-SABR cap occurs when `STREAM_PROTECTION_STATUS` stays 2
(attestation pending). The server delivers roughly the first minute of audio
(itag 258 is about 70s / 8 segments / 3.39 MB; itag 251 is about 60s / 6
segments) and then goes media-empty for the rest of the session, no matter how
the request advances.

It is **not** a PO-token problem and **not** a request-shape problem. Do not
re-chase either. The evidence:

- Token A/B (hold the request constant, vary only the GVS token): a real
  INTEGRITY mint, a garbage token, and an empty token all deliver byte-identical
  output and cap at the same segment. The server never consults the token.
- A warmed, residential, genuine-Chromium (Playwright) session playing the same
  video in YouTube's **own** web player hits the **identical** cap (itag 251:
  `duration_ms = 60001`, 6 segments) and then errors "Something went wrong." Its
  request contains many more fields than ours, so matching the shape would not
  help.
- Also ruled out: video (a concurrent video track does not lift the audio cap),
  anonymous cookies (googlevideo is a different eTLD+1; the browser sends none),
  residential egress IP, wall-clock pacing (capping reported
  `player_time_ms` to real elapsed and waiting 115s does not resume), readahead,
  audio format/bitrate, and client patience (polling 70+ rounds past the wall
  with raised guards never resumes).

The differentiator is client **genuineness**, scored upstream of the PO token:
automation markers (e.g. `navigator.webdriver`), a live in-context BotGuard, and
the transport (TLS/HTTP2) fingerprint. A client that fails this check is served
the ~1-minute preview; one that passes streams the full file. This is not about
"headless" per se: a *properly attested* browser passes even headless (WaxSeal's
mint browser, with `webdriver=false` and an in-context BotGuard bundle, streams
the whole video), while stock automation (Playwright with `webdriver=true`, no
in-context BotGuard) fails and shows the cap plus a "Something went wrong" error,
which is the failed-attestation signature, not a generic headless cap. A
browserless Go client cannot itself pass the gate (no in-context BotGuard;
real-Chrome TLS alone was insufficient in testing).

But the entitlement is a **transferable artifact**: status 1 is baked into the
signed `serverAbrStreamingUrl`'s grade, minted by an attested browser that has
**begun playback**, and that URL streams the full file from a plain cold Go client,
verified with the attesting daemon **stopped**, so it is not tethered to a live
session. So full WEB audio is reachable via the **attested `/player-context`
handoff** (see the README "PO tokens & WEB"): WaxTap consumes the context,
descrambles `n` with its `player_url`, binds a GVS token to its `visitorData`, and
streams through the normal SABR loop. Verified end to end on a fresh live mint:
full `634.624s`, itag 258, `status 1` every round, cold start.

Do not pursue token or request-shape changes for this cap. A WEB `/player` call
made by WaxTap, or any **bare** in-page `/player` fetch, receives only the
status-2 preview. The attested browser must begin playback before its
`serverAbrStreamingUrl` carries the status-1 grade; the load-time URL is
status-2. WaxTap classifies an un-handed-off status-2 stall token-neutrally as
`ErrIncompleteStream` ("...under attestation-pending (status 2); cause is upstream
of the PO token").

**ANDROID_VR is the default** (no gate, no GVS pot, direct signed URLs):
verified tokenless on `aqz-KE-bpKQ`: full file, `duration=634.624s`,
30,767,611 bytes, itag 258. WEB is opt-in via `--player-context-url` and falls
back to ANDROID_VR on failure, so the cap never blocks a download.

For a stall that does not match that signature, reproduce with `-v` and
capture stderr; optionally set `WAXTAP_SABR_DUMP_DIR` to keep each round's raw
response body:

```sh
WAXTAP_SABR_DUMP_DIR=/tmp/sabr waxtap download -v --client web \
  --potoken-url http://127.0.0.1:4416 --session-url http://127.0.0.1:4416/session \
  --dir /tmp/out "https://www.youtube.com/watch?v=VIDEO_ID" 2> /tmp/sabr.log
```

Read off the debug lines (`sabr: segment received`, `sabr: request state`,
`sabr: next request policy`, `sabr: round complete`):

- `duration_ms` 0 with a non-zero `effective_duration_ms`: the server moved
  the duration into `time_range` only. `downloadedMs` and the buffered-range
  acks both consume `effectiveDurationMs` (and `effectiveStartMs` for the
  range start), so this alone is informational, not a stall cause.
- `player_time_ms` in `sabr: request state` stops advancing: a duration source
  dried up; find which.
- After a freeze: re-sent segments below `next_seq` versus `media_parts` 0 in
  `sabr: round complete` separates re-serving from total silence.
- A mid-stream init whose `format_*` fields differ from the pinned format
  points to a server-side format switch.

The dump files (`<dir>/<timestamp>-sabr-round-NNN.bin`) hold the exact
UMP/protobuf bytes. Re-decode them offline with the integration-tagged helper,
which prints every UMP part (including ids the consumer skips) and walks each
protobuf payload field by field:

```sh
WAXTAP_SABR_DUMP_DECODE=/tmp/sabr go test -tags=integration \
  -run TestDecodeSABRDumps ./youtube/internal/sabr/ -v
```

Like `WAXTAP_DUMP_DIR`, the dump is best-effort and never changes stream
behavior.

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
