package waxtap

import (
	"io"
	"time"

	"github.com/colespringer/waxtap/format"
	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/sponsorblock"
	"github.com/colespringer/waxtap/youtube"
)

// ---------------------------------------------------------------------------
// Re-exported contracts (canonical definitions live in the owning packages).
// ---------------------------------------------------------------------------

// Audio format model and selectors (package format).
type (
	Format        = format.Format
	AudioTrack    = format.AudioTrack
	Tri           = format.Tri
	AudioSelector = format.AudioSelector
	SourcePolicy  = format.SourcePolicy
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

// BestAudio selects the best audio stream, preferring the original track,
// non-DRC audio, then higher effective bitrate.
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
)

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// ProcessSpec is the processing pipeline shared by YouTube and local-file
// requests. Each stage is opt-in: a nil pointer means that stage is skipped, so
// the default path keeps the selected source stream unchanged.
type ProcessSpec struct {
	Transcode *TranscodeSpec // nil = keep source, no re-encode
	Cut       *CutSpec       // nil = no cut
	Loudness  *LoudnessSpec  // nil = no loudness work

	// Output is the sink. For source-style delivery (an io.ReadCloser to pipe
	// elsewhere) use Client.Stream instead of setting Output.
	Output Output

	// Events receives best-effort, synchronous, panic-recovered stage events.
	// It may be nil. A slow callback backpressures the worker, so keep it fast.
	Events func(Event)

	// SkipIfExists skips work when the exact output path already exists. This is
	// only a path check; callers remain responsible for library-level deduping.
	SkipIfExists bool
}

// Request is a YouTube acquisition + processing request.
type Request struct {
	URL string

	// Audio selects which audio stream to take. The zero value is BestAudio.
	Audio AudioSelector
	// SourcePolicy controls the source tradeoff when transcoding. The zero value
	// is MinimizeLoss.
	SourcePolicy SourcePolicy

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

// ---------------------------------------------------------------------------
// Processing specs
// ---------------------------------------------------------------------------

// TranscodeFormat names an output preset. FormatCopy is the only no-re-encode
// path. FLAC, ALAC, and WAV preserve the decoded samples, but they are still
// decode-and-encode passes when the source is YouTube audio.
type TranscodeFormat uint8

const (
	FormatCopy TranscodeFormat = iota // remux / stream-copy (no re-encode)
	FormatFLAC
	FormatALAC
	FormatWAV
	FormatMP3
	FormatAAC // delivered in an .m4a container
	FormatOpus
	FormatVorbis
)

// TranscodeSpec requests ffmpeg processing. An explicit FormatCopy stream-copies
// through ffmpeg to remux into the destination container; a nil TranscodeSpec
// keeps the selected source bytes untouched.
type TranscodeSpec struct {
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
	// Ranges are explicit [Start, End) removals (optional).
	Ranges []TimeRange
	// SponsorBlock lists categories to fetch and remove. Nil disables the SB
	// fetch entirely; an empty-but-non-nil slice falls back to
	// sponsorblock.DefaultCategories.
	SponsorBlock []sponsorblock.Category
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

// LoudnessResult reports loudness measurements. WaxTap returns LUFS/true-peak
// measurements, not ReplayGain tag values.
type LoudnessResult struct {
	Input  *LoudnessInfo // measured input loudness (post-cut)
	Output *LoudnessInfo // post-apply loudness; set only when Mode == LoudnessApply
	Target float64
}

// TimeRange is a half-open [Start, End) span. End must be greater than Start.
type TimeRange struct {
	Start time.Duration
	End   time.Duration
}

// ---------------------------------------------------------------------------
// Output sink
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Results
// ---------------------------------------------------------------------------

// SourceKind distinguishes a YouTube download from local-file processing.
type SourceKind uint8

const (
	SourceYouTube SourceKind = iota
	SourceLocalFile
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
// completed effects, not requested work: an empty SponsorBlock match leaves
// SponsorBlockApplied false, and ranges that vanish after clamping leave
// CutApplied false.
type Result struct {
	SourceKind SourceKind
	VideoID    string // empty for local files
	Title      string // empty for local files
	InputPath  string // set for local files
	OutputPath string // empty for ToWriter delivery

	SourceFormat Format // input/source format
	OutputFormat Format // after transcode (== source when copy/keep)

	SourceBytes int64
	OutputBytes int64

	Transcoded          bool
	CutApplied          bool
	SponsorBlockApplied bool
	LoudnessMeasured    bool // measured != normalized
	LoudnessApplied     bool
	Loudness            *LoudnessResult // nil unless measured

	Warnings []Warning
}

// StreamInfo is the initial metadata returned by Client.Stream alongside the
// stream reader. Final byte counts are known only after read-to-EOF/Close.
type StreamInfo struct {
	VideoID       string
	Title         string
	Format        Format
	ContentLength int64 // 0 if unknown
}

// ---------------------------------------------------------------------------
// Enumeration
// ---------------------------------------------------------------------------

// EnumerateOptions tunes playlist enumeration. Enumeration never downloads.
type EnumerateOptions struct {
	// MaxItems caps the number of entries returned (0 = all).
	MaxItems int
	// Enrich is reserved for a future full-metadata pass. Current enumeration
	// returns lightweight entries regardless of this value.
	Enrich bool
}

// ---------------------------------------------------------------------------
// Events & warnings
// ---------------------------------------------------------------------------

// Stage identifies a pipeline stage in an Event.
type Stage uint8

const (
	StageExtracting Stage = iota
	StageResolving
	StageDownloading
	StageStaging
	StageProbing
	StageAnalyzing
	StageCutting
	StageNormalizing
	StageTranscoding
	StageFinalizing
	StageSkipped
	StageWarning
	StageDone
	StageFailed
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

// WarningCode is a stable, machine-actionable warning identifier. Warning.Detail
// is human-only.
type WarningCode uint8

const (
	WarnProceedUncut        WarningCode = iota // SponsorBlock fetch failed; delivered uncut
	WarnFallbackProfile                        // a fallback client profile was used
	WarnURLReResolved                          // an expired stream URL was re-resolved
	WarnPlaylistEntryFailed                    // one playlist entry failed (others returned)
	WarnRateLimitedRetried                     // a request was retried after a 429
	WarnSponsorBlockEmpty                      // SponsorBlock matched no segments
	WarnRangesEmpty                            // cut ranges were empty after clamp/merge
	WarnThrottled                              // a limiter/cooldown is active
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
	default:
		return "unknown"
	}
}

// Warning is a typed, non-fatal signal. It is both delivered as a StageWarning
// Event and accumulated in Result.Warnings.
type Warning struct {
	Code   WarningCode
	Detail string // human-readable context
}

// Event is a best-effort progress signal. Callbacks are invoked synchronously
// from the worker and are panic-recovered. A terminal event always fires:
// StageDone on success or StageFailed with Err. For Stream, the terminal event
// is emitted when the returned reader is closed.
type Event struct {
	Stage   Stage
	VideoID string

	// Downloading progress.
	Bytes int64
	Total int64 // 0 if unknown

	// CLI playlist expansion.
	ItemIndex int
	ItemCount int

	Warning *Warning // set when Stage == StageWarning
	Err     error    // set when Stage == StageFailed
	Message string
}
