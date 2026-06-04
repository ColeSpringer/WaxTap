package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/waxerr"
)

// ProbeResult is the parsed subset of ffprobe's -show_format -show_streams JSON:
// container metadata plus per-stream codec details.
type ProbeResult struct {
	Format  ProbeFormat
	Streams []ProbeStream
}

// ProbeFormat describes the container.
type ProbeFormat struct {
	FormatName string
	Duration   time.Duration
	Size       int64
	BitRate    int
}

// ProbeStream describes one media stream.
type ProbeStream struct {
	Index      int
	CodecType  string // "audio", "video", ...
	CodecName  string // e.g. "opus", "aac", "flac"
	SampleRate int    // Hz (audio)
	Channels   int
	BitRate    int
	Duration   time.Duration

	// SampleFmt is ffprobe's sample_fmt (for example "s16", "s32", or "fltp").
	// BitsPerSample is the coded PCM depth. BitsPerRawSample is the source depth
	// reported by some lossless codecs, such as a 24-bit FLAC. A zero value means
	// ffprobe did not report the field.
	SampleFmt        string
	BitsPerSample    int
	BitsPerRawSample int
}

// effectiveBits returns the best available integer bit depth. It prefers
// BitsPerRawSample for lossless codecs and falls back to BitsPerSample for PCM.
// A zero return means the depth is unknown.
func (s ProbeStream) effectiveBits() int {
	return max(s.BitsPerRawSample, s.BitsPerSample)
}

// AudioStream returns the first audio stream and true, or a zero stream and false
// when the result carries no audio. Probe rejects no-audio inputs with
// waxerr.ErrUnsupportedInput, so a result obtained from Probe always has one;
// this accessor is for inspecting a ProbeResult directly.
func (p ProbeResult) AudioStream() (ProbeStream, bool) {
	for _, s := range p.Streams {
		if s.CodecType == "audio" {
			return s, true
		}
	}
	return ProbeStream{}, false
}

// Probe runs ffprobe on a local input and returns the parsed result. ffprobe
// failures, malformed JSON, and media with no audio stream are reported as
// waxerr.ErrUnsupportedInput.
func (r *Runner) Probe(ctx context.Context, input string) (ProbeResult, error) {
	return r.probe(ctx, input, nil)
}

// ProbeURL probes a remote input with the supplied HTTP headers. This is used for
// signed media URLs whose User-Agent or token headers must match the resolver.
// Empty headers behave like Probe. ProbeURL can block on the network, so callers
// should pass a bounded context.
func (r *Runner) ProbeURL(ctx context.Context, url string, headers http.Header) (ProbeResult, error) {
	return r.probe(ctx, url, headers)
}

func (r *Runner) probe(ctx context.Context, input string, headers http.Header) (ProbeResult, error) {
	args := []string{"-hide_banner", "-loglevel", "error", "-print_format", "json", "-show_format", "-show_streams"}
	if h := formatProbeHeaders(headers); h != "" {
		// -headers is an input option, so it must precede the input argument.
		args = append(args, "-headers", h)
	}
	args = append(args, input)

	stdout, stderr, err := r.run(ctx, r.ffprobePath, args, true)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ProbeResult{}, ctxErr
		}
		return ProbeResult{}, fmt.Errorf("%w: ffprobe: %s", waxerr.ErrUnsupportedInput, stderrSummary(stderr, err))
	}
	pr, perr := parseProbe(stdout)
	if perr != nil {
		return ProbeResult{}, fmt.Errorf("%w: %v", waxerr.ErrUnsupportedInput, perr)
	}
	// ffprobe can succeed on video-only media. Classify that at the probe
	// boundary so callers see the same error they get for other unsupported
	// inputs.
	if _, ok := pr.AudioStream(); !ok {
		return ProbeResult{}, fmt.Errorf("%w: no audio stream", waxerr.ErrUnsupportedInput)
	}
	return pr, nil
}

// formatProbeHeaders renders HTTP headers as ffmpeg's -headers value: CRLF-
// terminated "Key: Value" lines. It returns "" for no headers.
func formatProbeHeaders(h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	var b strings.Builder
	for k, vs := range h {
		for _, v := range vs {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\r\n")
		}
	}
	return b.String()
}

// rawProbe mirrors ffprobe's JSON, where numeric fields are encoded as strings.
type rawProbe struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index      int    `json:"index"`
		CodecType  string `json:"codec_type"`
		CodecName  string `json:"codec_name"`
		SampleRate string `json:"sample_rate"`
		Channels   int    `json:"channels"`
		BitRate    string `json:"bit_rate"`
		Duration   string `json:"duration"`
		SampleFmt  string `json:"sample_fmt"`
		// ffprobe emits bits_per_sample as a number but bits_per_raw_sample as a
		// string, so they are decoded with different types.
		BitsPerSample    int    `json:"bits_per_sample"`
		BitsPerRawSample string `json:"bits_per_raw_sample"`
	} `json:"streams"`
}

func parseProbe(b []byte) (ProbeResult, error) {
	var raw rawProbe
	if err := json.Unmarshal(b, &raw); err != nil {
		return ProbeResult{}, fmt.Errorf("parse ffprobe json: %w", err)
	}
	pr := ProbeResult{
		Format: ProbeFormat{
			FormatName: raw.Format.FormatName,
			Duration:   parseSeconds(raw.Format.Duration),
			Size:       parseInt(raw.Format.Size),
			BitRate:    int(parseInt(raw.Format.BitRate)),
		},
	}
	for _, s := range raw.Streams {
		pr.Streams = append(pr.Streams, ProbeStream{
			Index:            s.Index,
			CodecType:        s.CodecType,
			CodecName:        s.CodecName,
			SampleRate:       int(parseInt(s.SampleRate)),
			Channels:         s.Channels,
			BitRate:          int(parseInt(s.BitRate)),
			Duration:         parseSeconds(s.Duration),
			SampleFmt:        s.SampleFmt,
			BitsPerSample:    s.BitsPerSample,
			BitsPerRawSample: int(parseInt(s.BitsPerRawSample)),
		})
	}
	return pr, nil
}

// parseInt parses an ffprobe integer field, returning 0 when absent ("N/A") or
// unparseable.
func parseInt(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// parseSeconds parses an ffprobe seconds-with-fraction field into a Duration,
// returning 0 when absent or unparseable.
func parseSeconds(s string) time.Duration {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}
