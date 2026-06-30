package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/colespringer/waxtap"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cacheSubdir is the WaxTap subdirectory under the user cache dir.
const cacheSubdir = "waxtap"

// Default per-operation timeouts. They bound extraction, resolving, and retry
// waits without putting a deadline on an entire large download.
const (
	defaultExtractionTimeout   = 45 * time.Second
	defaultResolveTimeout      = 30 * time.Second
	defaultWebContextTimeout   = 20 * time.Second
	defaultSponsorBlockTimeout = 10 * time.Second
	defaultChunkTimeout        = 120 * time.Second
	defaultFFmpegShutdown      = 5 * time.Second
)

// appConfig holds resolved configuration after merging flags, environment, and
// the optional JSON file. It is the single input used to build waxtap.Options.
type appConfig struct {
	json    bool
	quiet   bool
	verbose bool

	cacheDir string
	noCache  bool
	tempDir  string

	proxy    string
	insecure bool

	perHostQPS float64
	cooldown   time.Duration
	hl, gl     string

	ffmpegProcs      int
	chunks           int
	downloads        int
	sbBaseURL        string
	profileOverride  string
	chromeMajor      int
	potokenURL       string
	playerContextURL string
	client           string
	sessionURL       string
	visitorData      string
	cookiesPath      string
	apiKey           string

	channels string // default channel layout for download/transcode/cut
	downmix  bool   // default --downmix

	extractionTimeout   time.Duration
	resolveTimeout      time.Duration
	webContextTimeout   time.Duration
	sponsorBlockTimeout time.Duration
	chunkTimeout        time.Duration
}

// fileConfig mirrors the JSON config file. Pointer fields distinguish an absent
// key from a zero value, so a file does not override defaults it does not mention.
type fileConfig struct {
	CacheDir            *string  `json:"cacheDir"`
	NoCache             *bool    `json:"noCache"`
	TempDir             *string  `json:"tempDir"`
	Proxy               *string  `json:"proxy"`
	Insecure            *bool    `json:"insecure"`
	PerHostQPS          *float64 `json:"perHostQPS"`
	CooldownSec         *float64 `json:"cooldownSeconds"`
	HL                  *string  `json:"hl"`
	GL                  *string  `json:"gl"`
	FFmpegProcs         *int     `json:"ffmpegProcs"`
	Chunks              *int     `json:"chunkParallelism"`
	Downloads           *int     `json:"downloadConcurrency"`
	SponsorBlockBaseURL *string  `json:"sponsorBlockBaseURL"`
	ProfileOverridePath *string  `json:"profileOverridePath"`
	ChromeMajor         *int     `json:"chromeMajor"`
	POTokenURL          *string  `json:"poTokenURL"`
	PlayerContextURL    *string  `json:"playerContextURL"`
	Client              *string  `json:"client"`
	SessionURL          *string  `json:"sessionURL"`
	VisitorData         *string  `json:"visitorData"`
	CookiesPath         *string  `json:"cookies"`
	APIKey              *string  `json:"apiKey"`
	Channels            *string  `json:"channels"`
	Downmix             *bool    `json:"downmix"`

	ExtractionTimeoutSec   *float64 `json:"extractionTimeoutSeconds"`
	ResolveTimeoutSec      *float64 `json:"resolveTimeoutSeconds"`
	WebContextTimeoutSec   *float64 `json:"webContextTimeoutSeconds"`
	SponsorBlockTimeoutSec *float64 `json:"sponsorBlockTimeoutSeconds"`
	ChunkTimeoutSec        *float64 `json:"chunkTimeoutSeconds"`
}

// loadConfig resolves configuration with precedence: an explicit flag wins, then
// a WAXTAP_* environment variable, then the JSON config file, then a built-in
// default.
func loadConfig(cmd *cobra.Command) (*appConfig, error) {
	fc, err := readConfigFile(cmd)
	if err != nil {
		return nil, err
	}
	ec, err := envOverlay()
	if err != nil {
		return nil, err
	}

	flags := cmd.Flags()
	str := func(name string, file, env *string, def string) string {
		return coalesceString(def, file, env, flagPtr(flags, name))
	}
	boolean := func(name string, file, env *bool, def bool) bool {
		return coalesceBool(def, file, env, flagBoolPtr(flags, name))
	}

	a := &appConfig{
		json:    rootFlagsValue.json,
		quiet:   rootFlagsValue.quiet,
		verbose: rootFlagsValue.verbose,

		cacheDir: str("cache-dir", fc.CacheDir, ec.CacheDir, ""),
		noCache:  boolean("no-cache", fc.NoCache, ec.NoCache, false),
		tempDir:  str("temp-dir", fc.TempDir, ec.TempDir, ""),

		proxy:    str("proxy", fc.Proxy, ec.Proxy, ""),
		insecure: boolean("insecure", fc.Insecure, ec.Insecure, false),

		perHostQPS: coalesceFloat(0, fc.PerHostQPS, ec.PerHostQPS, flagFloatPtr(flags, "qps")),
		cooldown:   coalesceDuration(0, fc.CooldownSec, ec.CooldownSec, flagDurationPtr(flags, "cooldown")),
		hl:         str("hl", fc.HL, ec.HL, ""),
		gl:         str("gl", fc.GL, ec.GL, ""),

		ffmpegProcs:      coalesceInt(0, fc.FFmpegProcs, ec.FFmpegProcs, nil),
		chunks:           coalesceInt(0, fc.Chunks, ec.Chunks, nil),
		downloads:        coalesceInt(0, fc.Downloads, ec.Downloads, nil),
		sbBaseURL:        str("sponsorblock-url", fc.SponsorBlockBaseURL, ec.SponsorBlockBaseURL, ""),
		profileOverride:  str("profile-override", fc.ProfileOverridePath, ec.ProfileOverridePath, ""),
		chromeMajor:      coalesceInt(0, fc.ChromeMajor, ec.ChromeMajor, flagIntPtr(flags, "chrome-major")),
		potokenURL:       str("potoken-url", fc.POTokenURL, ec.POTokenURL, ""),
		playerContextURL: str("player-context-url", fc.PlayerContextURL, ec.PlayerContextURL, ""),
		client:           str("client", fc.Client, ec.Client, ""),
		sessionURL:       str("session-url", fc.SessionURL, ec.SessionURL, ""),
		visitorData:      str("visitor-data", fc.VisitorData, ec.VisitorData, ""),
		cookiesPath:      str("cookies", fc.CookiesPath, ec.CookiesPath, ""),
		apiKey:           str("api-key", fc.APIKey, ec.APIKey, ""),

		// These are command flags, so configuration applies only when the
		// corresponding flag is unset.
		channels: coalesceString("", fc.Channels, ec.Channels),
		downmix:  coalesceBool(false, fc.Downmix, ec.Downmix),

		extractionTimeout:   coalesceDuration(defaultExtractionTimeout, fc.ExtractionTimeoutSec, ec.ExtractionTimeoutSec),
		resolveTimeout:      coalesceDuration(defaultResolveTimeout, fc.ResolveTimeoutSec, ec.ResolveTimeoutSec),
		webContextTimeout:   coalesceDuration(defaultWebContextTimeout, fc.WebContextTimeoutSec, ec.WebContextTimeoutSec),
		sponsorBlockTimeout: coalesceDuration(defaultSponsorBlockTimeout, fc.SponsorBlockTimeoutSec, ec.SponsorBlockTimeoutSec),
		chunkTimeout:        coalesceDuration(defaultChunkTimeout, fc.ChunkTimeoutSec, ec.ChunkTimeoutSec),
	}
	if err := validateLocale(a.hl, a.gl); err != nil {
		return nil, err
	}
	return a, nil
}

// hlPattern and glPattern validate the locale forms accepted by YouTube: a
// BCP-47-style language tag for hl and a two-letter region for gl. They validate
// syntax only and intentionally do not maintain an ISO code list.
var (
	hlPattern = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)
	glPattern = regexp.MustCompile(`^[A-Za-z]{2}$`)
)

// validateLocale checks the resolved --hl and --gl values. Empty values are
// valid because the YouTube client applies its own defaults.
func validateLocale(hl, gl string) error {
	if hl != "" && !hlPattern.MatchString(hl) {
		return usagef("invalid --hl %q (want a language code like en or pt-BR)", hl)
	}
	if gl != "" && !glPattern.MatchString(gl) {
		return usagef("invalid --gl %q (want a two-letter region code like US)", gl)
	}
	return nil
}

// readConfigFile loads the JSON config file: the --config flag, then
// WAXTAP_CONFIG, then config.json in the user config dir. A missing default file
// is not an error; an explicitly named file that is missing or malformed is.
func readConfigFile(cmd *cobra.Command) (fileConfig, error) {
	var fc fileConfig
	path, _ := cmd.Flags().GetString("config")
	// Only --config requires the named file to exist. Missing environment and
	// default paths use built-in defaults, while malformed files still return an
	// error.
	flagExplicit := cmd.Flags().Changed("config")
	if path == "" {
		if env := os.Getenv("WAXTAP_CONFIG"); env != "" {
			path = env
		} else if dir, err := os.UserConfigDir(); err == nil {
			path = filepath.Join(dir, cacheSubdir, "config.json")
		}
	}
	if path == "" {
		return fc, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !flagExplicit {
			return fc, nil // optional file is absent; use defaults
		}
		return fc, usagef("read config %s: %v", path, err)
	}
	// Treat misspelled keys as configuration errors, matching the environment
	// overlay's strict parsing.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		// json.UnmarshalTypeError exposes the JSON value kind. Use that instead of
		// the default message, which includes internal Go type names.
		if uterr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			if uterr.Field == "" {
				return fc, usagef("parse config %s: expected a JSON object, got %s", path, uterr.Value)
			}
			return fc, usagef("parse config %s: field %q has the wrong type (got %s)", path, uterr.Field, uterr.Value)
		}
		return fc, usagef("parse config %s: %v", path, err)
	}
	// Reject content after the first JSON object. Trailing whitespace is allowed.
	if dec.More() {
		return fc, usagef("parse config %s: unexpected trailing data after the JSON object", path)
	}
	return fc, nil
}

// envOverlay reads WAXTAP_* environment variables into a fileConfig-shaped
// overlay. Malformed numeric/boolean values are reported rather than silently
// ignored, so a typo surfaces.
func envOverlay() (fileConfig, error) {
	var ec fileConfig
	var errs []error
	getStr := func(key string) *string {
		if v, ok := os.LookupEnv(key); ok {
			return &v
		}
		return nil
	}
	getBool := func(key string) *bool {
		v, ok := os.LookupEnv(key)
		if !ok {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return nil
		}
		return &b
	}
	getFloat := func(key string) *float64 {
		v, ok := os.LookupEnv(key)
		if !ok {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return nil
		}
		return &f
	}
	getInt := func(key string) *int {
		v, ok := os.LookupEnv(key)
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return nil
		}
		return &n
	}

	ec.CacheDir = getStr("WAXTAP_CACHE_DIR")
	ec.NoCache = getBool("WAXTAP_NO_CACHE")
	ec.TempDir = getStr("WAXTAP_TEMP_DIR")
	ec.Proxy = getStr("WAXTAP_PROXY")
	ec.Insecure = getBool("WAXTAP_INSECURE")
	ec.PerHostQPS = getFloat("WAXTAP_QPS")
	ec.CooldownSec = getFloat("WAXTAP_COOLDOWN")
	ec.HL = getStr("WAXTAP_HL")
	ec.GL = getStr("WAXTAP_GL")
	ec.FFmpegProcs = getInt("WAXTAP_FFMPEG_PROCS")
	ec.Chunks = getInt("WAXTAP_CHUNKS")
	ec.Downloads = getInt("WAXTAP_DOWNLOAD_CONCURRENCY")
	ec.SponsorBlockBaseURL = getStr("WAXTAP_SPONSORBLOCK_BASE_URL")
	ec.ProfileOverridePath = getStr("WAXTAP_PROFILE_OVERRIDE")
	ec.ChromeMajor = getInt("WAXTAP_CHROME_MAJOR")
	ec.POTokenURL = getStr("WAXTAP_POTOKEN_URL")
	ec.PlayerContextURL = getStr("WAXTAP_PLAYER_CONTEXT_URL")
	ec.Client = getStr("WAXTAP_CLIENT")
	ec.SessionURL = getStr("WAXTAP_SESSION_URL")
	ec.VisitorData = getStr("WAXTAP_VISITOR_DATA")
	ec.CookiesPath = getStr("WAXTAP_COOKIES")
	ec.APIKey = getStr("WAXTAP_API_KEY")
	ec.Channels = getStr("WAXTAP_CHANNELS")
	ec.Downmix = getBool("WAXTAP_DOWNMIX")
	ec.ExtractionTimeoutSec = getFloat("WAXTAP_EXTRACTION_TIMEOUT")
	ec.ResolveTimeoutSec = getFloat("WAXTAP_RESOLVE_TIMEOUT")
	ec.WebContextTimeoutSec = getFloat("WAXTAP_WEB_CONTEXT_TIMEOUT")
	ec.SponsorBlockTimeoutSec = getFloat("WAXTAP_SPONSORBLOCK_TIMEOUT")
	ec.ChunkTimeoutSec = getFloat("WAXTAP_CHUNK_TIMEOUT")

	if len(errs) > 0 {
		return ec, usagef("invalid environment configuration: %v", errors.Join(errs...))
	}
	return ec, nil
}

// resolveChannelsFlag returns the explicit --channels value when set, otherwise
// the configured default or the command's built-in default.
func resolveChannelsFlag(cmd *cobra.Command, cfg *appConfig, channels string) string {
	if !cmd.Flags().Changed("channels") && cfg.channels != "" {
		return cfg.channels
	}
	return channels
}

// resolveChannels returns and validates the effective channel layout and
// downmix setting for a processing command.
func resolveChannels(cmd *cobra.Command, cfg *appConfig, channels string, downmix bool) (waxtap.ChannelLayout, bool, error) {
	channels = resolveChannelsFlag(cmd, cfg, channels)
	if !cmd.Flags().Changed("downmix") && cfg.downmix {
		downmix = true
	}
	return channelsAndDownmix(channels, downmix)
}

// resolvedCacheDir returns the effective on-disk cache directory: the configured
// path, or os.UserCacheDir()/waxtap when unset.
func (a *appConfig) resolvedCacheDir() (string, error) {
	if a.cacheDir != "" {
		return a.cacheDir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache dir: %w", err)
	}
	return filepath.Join(base, cacheSubdir), nil
}

// options builds waxtap.Options from the resolved config and a logger.
func (a *appConfig) options(log *slog.Logger) (waxtap.Options, error) {
	hc, err := a.httpClient()
	if err != nil {
		return waxtap.Options{}, err
	}
	// A configured PO-token URL enables WEB/GVS tokens. The provider uses its own
	// dedicated client (see bgutilProvider), not hc, so token traffic is never
	// proxied through --proxy/--insecure.
	var poProvider waxtap.POTokenProvider
	if a.potokenURL != "" {
		endpoint, err := buildSidecarURL(a.potokenURL, "/get_pot")
		if err != nil {
			return waxtap.Options{}, usagef("invalid --potoken-url %q: %v", a.potokenURL, err)
		}
		poProvider = newBgutilProvider(endpoint, a.apiKey)
	}

	// The attested WEB /player-context path streams full WEB audio Go-side. It
	// binds a GVS PO token to the context's visitorData, so it requires a token
	// provider alongside it. Its own dedicated client is never proxied.
	var pcProvider waxtap.PlayerContextProvider
	if a.playerContextURL != "" {
		if a.potokenURL == "" {
			return waxtap.Options{}, usagef("--player-context-url requires --potoken-url (the WEB stream needs a GVS PO token bound to the context's visitorData)")
		}
		endpoint, err := buildSidecarURL(a.playerContextURL, "/player-context")
		if err != nil {
			return waxtap.Options{}, usagef("invalid --player-context-url %q: %v", a.playerContextURL, err)
		}
		pcProvider = newPlayerContextProvider(endpoint, a.apiKey)
	}

	// External session adoption: a pull-based --session-url provider, or a static
	// --visitor-data (+ optional --cookies) session. New enforces the uniform-chain
	// requirement and the Session/SessionProvider exclusivity.
	session, sessionProvider, err := a.externalSession()
	if err != nil {
		return waxtap.Options{}, err
	}

	return waxtap.Options{
		HTTPClient:            hc,
		Logger:                log,
		Locale:                waxtap.Locale{HL: a.hl, GL: a.gl},
		CacheDir:              a.cacheDir,
		DisableDiskCache:      a.noCache,
		TempDir:               a.tempDir,
		ProfileOverridePath:   a.profileOverride,
		ChromeMajor:           a.chromeMajor,
		POTokenProvider:       poProvider,
		PlayerContextProvider: pcProvider,
		Client:                a.client,
		Session:               session,
		SessionProvider:       sessionProvider,
		Concurrency: waxtap.Concurrency{
			Downloads: a.downloads,
			Chunks:    a.chunks,
			FFmpeg:    a.ffmpegProcs,
		},
		Timeouts: waxtap.Timeouts{
			Extraction:     a.extractionTimeout,
			Resolve:        a.resolveTimeout,
			WebContext:     a.webContextTimeout,
			SponsorBlock:   a.sponsorBlockTimeout,
			ChunkRetry:     a.chunkTimeout,
			FFmpegShutdown: defaultFFmpegShutdown,
		},
		Retry: waxtap.RetryPolicy{
			MaxRetries:   3,
			BaseBackoff:  500 * time.Millisecond,
			MaxBackoff:   10 * time.Second,
			MaxRetryWait: 60 * time.Second,
		},
		Politeness:   waxtap.Politeness{PerHostQPS: a.perHostQPS, Cooldown: a.cooldown},
		SponsorBlock: waxtap.SponsorBlockOptions{BaseURL: a.sbBaseURL},
	}, nil
}

// externalSession builds the adopted-session inputs: a pull-based --session-url
// provider, or a static --visitor-data (+ optional --cookies) session. The two
// sources are mutually exclusive, and --cookies requires --visitor-data because
// adoption skips the bootstrap that would otherwise supply visitorData.
func (a *appConfig) externalSession() (*waxtap.POTokenSession, waxtap.POTokenSessionProvider, error) {
	switch {
	case a.sessionURL != "":
		if a.visitorData != "" || a.cookiesPath != "" {
			return nil, nil, usagef("--session-url cannot be combined with --visitor-data/--cookies")
		}
		endpoint, err := buildSidecarURL(a.sessionURL, "/session")
		if err != nil {
			return nil, nil, usagef("invalid --session-url %q: %v", a.sessionURL, err)
		}
		return nil, newHTTPSessionProvider(endpoint, a.apiKey), nil
	case a.visitorData != "" || a.cookiesPath != "":
		if a.visitorData == "" {
			return nil, nil, usagef("--cookies requires --visitor-data: adoption needs the browser's exact visitorData")
		}
		var cookies []*http.Cookie
		if a.cookiesPath != "" {
			parsed, err := parseNetscapeCookies(a.cookiesPath)
			if err != nil {
				// An unreadable --cookies file is invalid CLI input. The underlying
				// error already identifies the file.
				return nil, nil, usagef("%s", err)
			}
			cookies = parsed
		}
		return &waxtap.POTokenSession{VisitorData: a.visitorData, Cookies: cookies}, nil, nil
	default:
		return nil, nil, nil
	}
}

// httpClient builds a custom base client only when transport settings require
// one. Returning nil lets the facade install its default jar-backed client.
func (a *appConfig) httpClient() (*http.Client, error) {
	if a.proxy == "" && !a.insecure {
		return nil, nil
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if a.proxy != "" {
		u, err := url.Parse(a.proxy)
		if err != nil {
			return nil, usagef("invalid --proxy %q: %v", a.proxy, err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	if a.insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit, diagnostics-only opt-in
	}
	// The jar keeps the guest-session bootstrap cookies available behind a proxy.
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: tr, Jar: jar}, nil
}

func coalesceString(def string, layers ...*string) string {
	v := def
	for _, l := range layers {
		if l != nil {
			v = *l
		}
	}
	return v
}

func coalesceBool(def bool, layers ...*bool) bool {
	v := def
	for _, l := range layers {
		if l != nil {
			v = *l
		}
	}
	return v
}

func coalesceFloat(def float64, layers ...*float64) float64 {
	v := def
	for _, l := range layers {
		if l != nil {
			v = *l
		}
	}
	return v
}

func coalesceInt(def int, layers ...*int) int {
	v := def
	for _, l := range layers {
		if l != nil {
			v = *l
		}
	}
	return v
}

// coalesceDuration treats the layered values as seconds.
func coalesceDuration(def time.Duration, layers ...*float64) time.Duration {
	v := def
	for _, l := range layers {
		if l != nil {
			v = time.Duration(*l * float64(time.Second))
		}
	}
	return v
}

// flagPtr returns the current flag value only when the user set the flag.
// Reading from the FlagSet lets each command own its flag storage. A flag that
// is absent from the set is treated as unset.
func flagPtr(flags *pflag.FlagSet, name string) *string {
	if !flags.Changed(name) {
		return nil
	}
	v, _ := flags.GetString(name)
	return &v
}

func flagBoolPtr(flags *pflag.FlagSet, name string) *bool {
	if !flags.Changed(name) {
		return nil
	}
	v, _ := flags.GetBool(name)
	return &v
}

func flagFloatPtr(flags *pflag.FlagSet, name string) *float64 {
	if !flags.Changed(name) {
		return nil
	}
	v, _ := flags.GetFloat64(name)
	return &v
}

func flagIntPtr(flags *pflag.FlagSet, name string) *int {
	if !flags.Changed(name) {
		return nil
	}
	v, _ := flags.GetInt(name)
	return &v
}

// flagDurationPtr returns an explicitly set duration in seconds, as expected by
// coalesceDuration.
func flagDurationPtr(flags *pflag.FlagSet, name string) *float64 {
	if !flags.Changed(name) {
		return nil
	}
	d, _ := flags.GetDuration(name)
	s := d.Seconds()
	return &s
}
