package waxtap

import (
	"encoding/json"
	"io"
	"math"
	"time"

	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/sponsorblock"
	"github.com/colespringer/waxtap/youtube"
)

// Audio format model and selectors (package format).
type (
	Format           = format.Format
	AudioTrack       = format.AudioTrack
	Tri              = format.Tri
	AudioQualityTier = format.AudioQualityTier
	AudioSelector    = format.AudioSelector
	ChannelLayout    = format.ChannelLayout
	SourcePolicy     = format.SourcePolicy
	// Target describes a transcode output for source selection. The facade maps
	// a TranscodeSpec onto it; most callers do not construct one directly.
	Target = format.Target
)

// ErrNoMatch reports that audio selection found no candidate satisfying the
// request. Download/Process translate it to ErrNoAudioFormats; it is re-exported
// for callers using BestForTarget directly.
var ErrNoMatch = format.ErrNoMatch

// BestForTarget chooses the best source audio index for a transcode target under
// a SourcePolicy. It is the selection BestAudio uses; exposed for callers that
// resolve formats themselves.
func BestForTarget(candidates []Format, policy SourcePolicy, target Target) (int, error) {
	return format.BestForTarget(candidates, policy, target)
}

// Tri values.
const (
	Unknown = format.Unknown
	Yes     = format.Yes
	No      = format.No
)

// Audio quality tiers reported by YouTube.
const (
	QualityUnknown  = format.QualityUnknown
	QualityUltraLow = format.QualityUltraLow
	QualityLow      = format.QualityLow
	QualityMedium   = format.QualityMedium
	QualityHigh     = format.QualityHigh
)

// Channel layouts used by AudioSelector.WithChannels and ProcessSpec.Channels.
// LayoutAny is the neutral zero value.
const (
	LayoutAny      = format.LayoutAny
	LayoutMono     = format.LayoutMono
	LayoutStereo   = format.LayoutStereo
	LayoutSurround = format.LayoutSurround
)

// BestAudio selects the best audio stream. It prefers the original track,
// non-DRC audio, higher reported quality tiers, Opus within a tier, and finally
// higher effective bitrate.
//
// With no channel preference it may rank a surround track highest. The CLI
// constrains this by defaulting to --channels stereo; library callers that want
// the same should call WithChannels(LayoutStereo).
func BestAudio() AudioSelector { return format.BestAudio() }

// Itag selects the stream with the exact itag.
func Itag(itag int) AudioSelector { return format.Itag(itag) }

// Codec selects the best stream whose codec matches (e.g. "opus", "aac").
func Codec(codec string) AudioSelector { return format.Codec(codec) }

// MinimizeLoss prefers a source in the target codec family, avoiding a
// cross-codec transcode when possible.
func MinimizeLoss() SourcePolicy { return format.MinimizeLoss() }

// BestNative ignores target codec matching and uses normal best-audio ranking.
func BestNative() SourcePolicy { return format.BestNative() }

// PreferCodec prefers a source in the named codec family when policy is active.
func PreferCodec(codec string) SourcePolicy { return format.PreferCodec(codec) }

// SponsorBlock types are re-exported so callers can configure [CutSpec] and call
// [Client.SponsorBlockSegments] without importing package sponsorblock.
type (
	// Category identifies a SponsorBlock segment category.
	Category = sponsorblock.Category
	// Segment describes one SponsorBlock skip segment.
	Segment = sponsorblock.Segment
)

// SponsorBlock categories. Values match the SponsorBlock API wire strings.
const (
	CategorySponsor       = sponsorblock.CategorySponsor
	CategorySelfPromo     = sponsorblock.CategorySelfPromo
	CategoryInteraction   = sponsorblock.CategoryInteraction
	CategoryIntro         = sponsorblock.CategoryIntro
	CategoryOutro         = sponsorblock.CategoryOutro
	CategoryPreview       = sponsorblock.CategoryPreview
	CategoryFiller        = sponsorblock.CategoryFiller
	CategoryMusicOffTopic = sponsorblock.CategoryMusicOffTopic
)

// DefaultCategories contains the categories used when [CutSpec.SponsorBlock] is
// a non-nil empty slice.
var DefaultCategories = sponsorblock.DefaultCategories

// Extraction models (package youtube). Part of the volatile surface; may evolve
// pre-1.0.
type (
	Video          = youtube.Video
	Thumbnail      = youtube.Thumbnail
	Chapter        = youtube.Chapter
	Playlist       = youtube.Playlist
	PlaylistEntry  = youtube.PlaylistEntry
	ResolvedStream = youtube.ResolvedStream
)

// PO-token provider contract (package potoken).
type (
	POTokenProvider = potoken.Provider
	POTokenRequest  = potoken.Request
	POTokenResponse = potoken.Response
	POTokenScope    = potoken.Scope
	// POTokenProviderFunc adapts a closure to POTokenProvider.
	POTokenProviderFunc = potoken.ProviderFunc
	// POTokenFailure describes the HTTP failure that triggered a token refresh.
	// [POTokenRequest].Failure points to a POTokenFailure when one is available.
	POTokenFailure = potoken.HTTPFailure
)

// PO-token scopes identify where a token will be used. Tokens are not
// interchangeable across scopes.
const (
	ScopeNone      = potoken.ScopeNone      // no token scope
	ScopePlayer    = potoken.ScopePlayer    // /player request body
	ScopeGVS       = potoken.ScopeGVS       // googlevideo media URL
	ScopeSubtitles = potoken.ScopeSubtitles // subtitle or timed-text URL
)

// External guest-session adoption (package potoken). A POTokenSession lets WaxTap
// adopt an externally supplied visitorData + cookies verbatim instead of
// bootstrapping its own, for byte-exact session coherence with a PO-token minter.
// POTokenSessionProvider is its pull-based form.
type (
	POTokenSession         = potoken.Session
	POTokenSessionProvider = potoken.SessionProvider
)

// Attested WEB player-context handoff (package potoken). A PlayerContextProvider
// supplies an attested /player streaming context (serverAbrStreamingUrl,
// ustreamer config, visitorData, and audio formats) that WaxTap streams Go-side,
// enabling the opt-in WEB SABR audio path.
type (
	PlayerContextProvider     = potoken.PlayerContextProvider
	PlayerContext             = potoken.PlayerContext
	PlayerContextFormat       = potoken.PlayerContextFormat
	PlayerContextProviderFunc = potoken.PlayerContextProviderFunc
)

// ProcessSpec is the processing pipeline shared by YouTube and local-file
// requests. Each stage is opt-in: a nil pointer means that stage is skipped, so
// the default path keeps the selected source stream unchanged.
type ProcessSpec struct {
	Transcode *TranscodeSpec // nil = keep source, no re-encode
	Cut       *CutSpec       // nil = no cut
	Loudness  *LoudnessSpec  // nil = no loudness work

	// Channels is the Downmix target layout. When Downmix is set it must be
	// LayoutMono or LayoutStereo; pairing Downmix with LayoutAny is a hard error.
	// LayoutAny, the zero value, means no downmix target, so Downmix must be false
	// and a surround source is delivered with all its channels (the CLI instead
	// defaults to --channels stereo). For YouTube requests, set the same preference
	// on Audio with WithChannels to favor a native track before processing.
	Channels ChannelLayout
	// Downmix reduces a source with more channels to Channels after probing. It
	// never adds channels and does nothing when the source already fits the
	// requested layout. Channels must be LayoutMono or LayoutStereo. When
	// Transcode is nil, the encoder is chosen from the source codec and destination
	// container.
	Downmix bool

	// Output is the sink. For source-style delivery (an io.ReadCloser to pipe
	// elsewhere) use Client.Stream instead of setting Output.
	Output Output

	// Events receives best-effort, synchronous, panic-recovered stage events.
	// It may be nil. A slow callback backpressures the worker, so keep it fast.
	Events func(Event)

	// SkipIfExists skips work when the exact output path already exists. This is
	// only a path check; callers remain responsible for library-level deduping.
	SkipIfExists bool

	// IncludeMetadata attaches extended video metadata to Result.Metadata for
	// YouTube downloads. It has no effect on local-file processing.
	IncludeMetadata bool

	// Threads limits ffmpeg's worker threads for processing operations. Zero lets
	// ffmpeg choose.
	Threads int
}

// Request is a YouTube acquisition + processing request.
type Request struct {
	// URL is a YouTube video URL or bare video ID.
	URL string

	// Audio selects which audio stream to take. The zero value is BestAudio.
	Audio AudioSelector
	// SourcePolicy controls source selection when transcoding. The zero value is
	// MinimizeLoss.
	SourcePolicy SourcePolicy

	// NoFallback prevents fallback from a WEB player context to the configured
	// client chain, disables watch-page extraction, and prevents retrying another
	// client after an incomplete download. The configured extraction chain may
	// still select a working client. Set Options.Client to force a single client.
	// Read methods use WithNoFallback for the same behavior.
	NoFallback bool

	ProcessSpec
}

// ProcessRequest processes a local audio file through the same pipeline as a
// YouTube download (transcode/cut/normalize), with no YouTube access.
type ProcessRequest struct {
	// Input is the local file path. Reader-based inputs will use a separate
	// request type; non-seekable inputs are staged before processing.
	Input string

	ProcessSpec
}

// TranscodeFormat names an output preset. FormatCopy is the only no-re-encode
// path. FLAC, ALAC, and WAV preserve the decoded samples, but they are still
// decode-and-encode passes when the source is YouTube audio.
type TranscodeFormat uint8

const (
	FormatCopy   TranscodeFormat = iota // remux / stream-copy (no re-encode)
	FormatFLAC                          // FLAC lossless audio
	FormatALAC                          // Apple Lossless audio
	FormatWAV                           // uncompressed PCM in a WAV container
	FormatMP3                           // MP3 audio
	FormatAAC                           // delivered in an .m4a container
	FormatOpus                          // Opus audio
	FormatVorbis                        // Vorbis audio
)

// TranscodeSpec requests ffmpeg processing. An explicit FormatCopy stream-copies
// through ffmpeg to remux into the destination container; a nil TranscodeSpec
// keeps the selected source bytes untouched.
type TranscodeSpec struct {
	// Format selects the output preset.
	Format TranscodeFormat
	// Bitrate is the target bits per second for lossy presets (e.g. 256000).
	// Zero selects the preset default. Ignored by lossless presets.
	Bitrate int
}

// CutMode selects how cuts are rendered.
type CutMode uint8

const (
	// CutSmart copies when cutting alone (lossless, frame-boundary) and fuses the
	// cut into the transcode when one is requested. It avoids cut-then-transcode
	// workflows that would encode twice.
	CutSmart CutMode = iota
	// CutCopy forces stream-copy; it errors with ErrIncompatibleSpec when copy is
	// unsafe for the codec/container.
	CutCopy
	// CutAccurate decodes, cuts sample-exactly, and re-encodes.
	CutAccurate
)

// SponsorBlockErrorPolicy governs SponsorBlock fetch failures only (ffmpeg
// cut/transcode failures are always hard errors).
type SponsorBlockErrorPolicy uint8

const (
	// ProceedUncut logs a warning and delivers the full, uncut audio when the
	// SponsorBlock fetch fails or times out (the default).
	ProceedUncut SponsorBlockErrorPolicy = iota
	// FailDownload fails the whole request when the SponsorBlock fetch fails.
	FailDownload
)

// CutSpec describes time-range removal and/or SponsorBlock-driven cuts.
type CutSpec struct {
	// Ranges are explicit [Start, End) removals (optional). They are clamped to the
	// media duration. A request whose ranges all lie outside the media returns
	// ErrIncompatibleSpec; partial overlaps remain valid.
	Ranges []TimeRange
	// SponsorBlock lists categories to fetch and remove. Nil disables
	// SponsorBlock; a non-nil empty slice uses [DefaultCategories].
	SponsorBlock []Category
	// Mode selects copy/accurate/smart rendering.
	Mode CutMode
	// Crossfade, when > 0, applies a click-free crossfade at splice points. It
	// is OFF by default and orthogonal to Mode (accurate does not imply it).
	Crossfade time.Duration
	// OnError governs the SponsorBlock fetch only.
	OnError SponsorBlockErrorPolicy
	// Timeout is a strict cap on the SponsorBlock fetch.
	Timeout time.Duration
}

// LoudnessMode selects measurement vs. normalization.
type LoudnessMode uint8

const (
	// LoudnessMeasureOnly returns measurements without altering the audio.
	LoudnessMeasureOnly LoudnessMode = iota
	// LoudnessApply normalizes to Target, fused into the transcode pass. It
	// requires an encode, so it is rejected with FormatCopy or no transcode unless
	// an explicit output codec is given (ErrIncompatibleSpec).
	LoudnessApply
)

// LoudnessSpec requests loudness measurement or normalization (EBU R128).
type LoudnessSpec struct {
	// Mode selects measurement or applied normalization.
	Mode LoudnessMode
	// Target is the target integrated loudness in LUFS for Apply (e.g. -14). The
	// value is the caller's policy; WaxTap does not impose one.
	Target float64
}

// LoudnessInfo holds an EBU R128 measurement.
type LoudnessInfo struct {
	IntegratedLUFS float64 // integrated loudness, LUFS
	TruePeakDBTP   float64 // true peak, dBTP
	LRA            float64 // loudness range, LU
	Threshold      float64 // relative gating threshold, LUFS
}

// MarshalJSON encodes non-finite measurements as JSON null because encoding/json
// rejects NaN and Inf. Silent tracks can produce -Inf; using null keeps
// LoudnessInfo JSON-friendly for Measure and MeasureAlbum callers. Field names
// stay the exported struct names.
func (l LoudnessInfo) MarshalJSON() ([]byte, error) {
	finite := func(v float64) *float64 {
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return nil
		}
		return &v
	}
	return json.Marshal(struct {
		IntegratedLUFS *float64
		TruePeakDBTP   *float64
		LRA            *float64
		Threshold      *float64
	}{finite(l.IntegratedLUFS), finite(l.TruePeakDBTP), finite(l.LRA), finite(l.Threshold)})
}

// LoudnessResult reports loudness measurements. WaxTap returns LUFS/true-peak
// measurements, not ReplayGain tag values.
type LoudnessResult struct {
	Input  *LoudnessInfo // measured input loudness (post-cut)
	Output *LoudnessInfo // post-apply loudness; set only when Mode == LoudnessApply
	Target float64       // requested integrated loudness in LUFS
}

// TimeRange is a half-open [Start, End) span. End must be greater than Start.
type TimeRange struct {
	Start time.Duration // inclusive start offset
	End   time.Duration // exclusive end offset
}

type outputKind uint8

const (
	outputNone outputKind = iota
	outputFile
	outputWriter
)

// Output is a delivery sink: either a file path or a writer. The zero value is
// unset. Construct it with ToFile or ToWriter.
//
// The library writes the exact path given to ToFile (only a temp suffix and an
// atomic rename); filename templating, sanitization, and collision handling are
// the CLI's job, not the library's.
type Output struct {
	kind   outputKind
	path   string
	writer io.Writer
}

// ToFile delivers to an exact file path (written atomically via a temp + rename).
func ToFile(path string) Output { return Output{kind: outputFile, path: path} }

// ToWriter delivers to a caller-provided writer (bounded memory, no atomicity
// guarantee).
func ToWriter(w io.Writer) Output { return Output{kind: outputWriter, writer: w} }

// SourceKind distinguishes a YouTube download from local-file processing.
type SourceKind uint8

const (
	SourceYouTube   SourceKind = iota // media acquired from YouTube
	SourceLocalFile                   // media read from a local file
)

func (k SourceKind) String() string {
	switch k {
	case SourceLocalFile:
		return "local-file"
	default:
		return "youtube"
	}
}

// Result reports the outcome of a Download or Process. Boolean flags describe
// completed effects, not requested work. For example, a SponsorBlock request
// that matches no segments leaves SponsorBlockApplied and CutApplied false.
type Result struct {
	SourceKind SourceKind // identifies a YouTube or local-file source
	VideoID    string     // empty for local files
	Title      string     // empty for local files
	InputPath  string     // set for local files
	OutputPath string     // empty for ToWriter delivery
	Client     string     // YouTube client used, such as "ANDROID_VR"; empty for local files

	SourceFormat Format // input/source format
	OutputFormat Format // after transcode (== source when copy/keep)

	SourceBytes int64 // bytes read from the acquired or local source
	OutputBytes int64 // bytes delivered to the output sink

	Transcoded          bool            // audio was re-encoded or remuxed
	CutApplied          bool            // at least one time range was removed
	SponsorBlockApplied bool            // SponsorBlock contributed a removed range
	LoudnessMeasured    bool            // measured != normalized
	LoudnessApplied     bool            // normalization was applied
	Loudness            *LoudnessResult // nil unless measured

	Warnings []Warning // non-fatal conditions encountered during processing

	// Metadata contains extended video metadata when ProcessSpec.IncludeMetadata
	// is set. It is nil otherwise.
	Metadata *VideoMetadata
}

// VideoMetadata contains optional YouTube metadata that is not stored directly
// on Result.
type VideoMetadata struct {
	Author      string        // channel / uploader name
	Duration    time.Duration // video duration, 0 if unknown
	PublishDate time.Time     // publication date, zero if unknown
	Description string        // video description
	Formats     []Format      // full candidate audio (and incidental video) formats
}

// StreamInfo is the initial metadata returned by Client.Stream alongside the
// stream reader. Final byte counts are known only after read-to-EOF/Close.
type StreamInfo struct {
	VideoID       string // resolved YouTube video ID
	Title         string // extracted video title
	Format        Format // selected source format
	ContentLength int64  // 0 if unknown
	Client        string // YouTube client used, such as "ANDROID_VR"
}

// EnumerateOptions tunes playlist enumeration. Enumeration never downloads.
type EnumerateOptions struct {
	// MaxItems caps the number of entries returned (0 = all).
	MaxItems int
	// Enrich refreshes entries with InfoBasic calls made at bounded concurrency.
	// Successful calls update their entries; failures are added to Playlist.Errors.
	Enrich bool

	// OnProgress reports the running entry count after each playlist page. It is
	// optional and never triggers downloads.
	OnProgress func(items int)
	// OnEnrichProgress reports each completed InfoBasic refresh when Enrich is set.
	// Calls are serialized in increasing done-count order. The final call reaches
	// (total, total) unless context cancellation stops enrichment early.
	OnEnrichProgress func(done, total int)
}

// Stage identifies a pipeline stage in an Event.
type Stage uint8

const (
	StageExtracting  Stage = iota // fetching and parsing source metadata
	StageResolving                // resolving the selected media stream
	StageDownloading              // transferring source bytes
	StageStaging                  // preparing a local working file
	StageProbing                  // inspecting media with ffprobe
	StageAnalyzing                // measuring loudness
	StageCutting                  // removing time ranges
	StageNormalizing              // applying loudness normalization
	StageTranscoding              // encoding or remuxing audio
	StageFinalizing               // delivering the completed output
	StageSkipped                  // skipping work because output already exists
	StageWarning                  // reporting a non-fatal warning
	StageDone                     // reporting successful completion
	StageFailed                   // reporting terminal failure
)

func (s Stage) String() string {
	switch s {
	case StageExtracting:
		return "extracting"
	case StageResolving:
		return "resolving"
	case StageDownloading:
		return "downloading"
	case StageStaging:
		return "staging"
	case StageProbing:
		return "probing"
	case StageAnalyzing:
		return "analyzing"
	case StageCutting:
		return "cutting"
	case StageNormalizing:
		return "normalizing"
	case StageTranscoding:
		return "transcoding"
	case StageFinalizing:
		return "finalizing"
	case StageSkipped:
		return "skipped"
	case StageWarning:
		return "warning"
	case StageDone:
		return "done"
	case StageFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// WarningCode is a stable, machine-readable warning identifier. Warning.Detail
// is intended for people.
type WarningCode uint8

const (
	WarnProceedUncut        WarningCode = iota // SponsorBlock fetch failed; delivered uncut
	WarnFallbackProfile                        // a fallback client profile was used
	WarnURLReResolved                          // an expired stream URL was re-resolved
	WarnPlaylistEntryFailed                    // one playlist entry failed (others returned)
	WarnRateLimitedRetried                     // a request was retried after a 429
	WarnSponsorBlockEmpty                      // SponsorBlock matched no segments
	WarnRangesEmpty                            // SponsorBlock segments all fell outside the media
	WarnThrottled                              // a limiter/cooldown is active
	WarnWebContextFallback                     // WEB player-context failed; fell back to the configured chain
	WarnIncompleteFallback                     // a client returned an incomplete stream; switched clients
	WarnWebContextRetry                        // WEB player-context was capped (status 2); retried once with a fresh context
)

func (w WarningCode) String() string {
	switch w {
	case WarnProceedUncut:
		return "proceed-uncut"
	case WarnFallbackProfile:
		return "fallback-profile"
	case WarnURLReResolved:
		return "url-re-resolved"
	case WarnPlaylistEntryFailed:
		return "playlist-entry-failed"
	case WarnRateLimitedRetried:
		return "rate-limited-retried"
	case WarnSponsorBlockEmpty:
		return "sponsorblock-empty"
	case WarnRangesEmpty:
		return "ranges-empty"
	case WarnThrottled:
		return "throttled"
	case WarnWebContextFallback:
		return "web-context-fallback"
	case WarnIncompleteFallback:
		return "incomplete-fallback"
	case WarnWebContextRetry:
		return "web-context-retry"
	default:
		return "unknown"
	}
}

// Warning is a typed, non-fatal signal. It is both delivered as a StageWarning
// Event and accumulated in Result.Warnings.
type Warning struct {
	Code   WarningCode // stable machine-readable identifier
	Detail string      // human-readable context
}

// Event is a best-effort progress signal. Callbacks are invoked synchronously
// from the worker and are panic-recovered. A terminal event always fires:
// StageDone on success or StageFailed with Err. For Stream, the terminal event
// is emitted when the returned reader is closed.
type Event struct {
	Stage   Stage  // current pipeline stage
	VideoID string // empty for local-file processing

	// Downloading progress.
	Bytes int64
	Total int64 // 0 if unknown

	// CLI playlist expansion.
	ItemIndex int
	ItemCount int // total playlist entries, or 0 when unknown

	Warning *Warning // set when Stage == StageWarning
	Err     error    // set when Stage == StageFailed
	Message string   // optional human-readable detail
}
