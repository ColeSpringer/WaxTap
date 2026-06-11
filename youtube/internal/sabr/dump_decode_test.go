//go:build integration

// Offline decoder for WAXTAP_SABR_DUMP_DIR round dumps. Excluded from the
// default build; run with -tags=integration:
//
//	WAXTAP_SABR_DUMP_DECODE=<dir-or-file> go test -tags=integration -run TestDecodeSABRDumps ./youtube/internal/sabr/ -v
//
// Every UMP part is printed, including ids the stream consumer skips, and
// protobuf payloads are walked field by field so unknown fields inside known
// messages are visible too.
package sabr

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

var umpPartNames = map[int]string{
	partMediaHeader:        "MEDIA_HEADER",
	partMedia:              "MEDIA",
	partMediaEnd:           "MEDIA_END",
	partNextRequestPolicy:  "NEXT_REQUEST_POLICY",
	partFormatInitMetadata: "FORMAT_INITIALIZATION_METADATA",
	partSabrRedirect:       "SABR_REDIRECT",
	partSabrError:          "SABR_ERROR",
	partReloadPlayerResp:   "RELOAD_PLAYER_RESPONSE",
	partSabrContextUpdate:  "SABR_CONTEXT_UPDATE",
	partStreamProtection:   "STREAM_PROTECTION_STATUS",
	partSabrContextSendPol: "SABR_CONTEXT_SENDING_POLICY",
}

func TestDecodeSABRDumps(t *testing.T) {
	path := os.Getenv("WAXTAP_SABR_DUMP_DECODE")
	if path == "" {
		t.Skip("set WAXTAP_SABR_DUMP_DECODE=<dir-or-file> to decode round dumps")
	}
	files := []string{path}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Fatal(err)
		}
		files = files[:0]
		for _, e := range entries {
			if !e.IsDir() {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
		sort.Strings(files)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("== %s (%d bytes)", filepath.Base(f), len(body))
		// Request bodies are a bare VideoPlaybackAbrRequest protobuf, not UMP.
		if strings.Contains(filepath.Base(f), "request") {
			for _, line := range protoTree(body, 0) {
				t.Log(line)
			}
			continue
		}
		r := newUMPReader(body)
		for {
			part, ok, err := r.next()
			if err != nil {
				t.Errorf("  UMP error: %v", err)
				break
			}
			if !ok {
				break
			}
			name := umpPartNames[part.Type]
			if name == "" {
				name = "UNKNOWN"
			}
			switch part.Type {
			case partMedia:
				id, media, err := leadingVarint(part.Payload)
				if err != nil {
					t.Errorf("  MEDIA: bad leading varint: %v", err)
					continue
				}
				t.Logf("  part %d %s: header_id=%d media=%dB", part.Type, name, id, len(media))
			default:
				t.Logf("  part %d %s: %dB%s", part.Type, name, len(part.Payload), protoFieldSummary(part.Payload))
			}
		}
	}
}

// protoFieldSummary renders a payload as a one-level protobuf field walk, or
// a hex preview when it does not parse as protobuf.
func protoFieldSummary(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var sb strings.Builder
	rest := b
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return fmt.Sprintf(" raw=%x", preview(b, 64))
		}
		rest = rest[n:]
		switch typ {
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(rest)
			if m < 0 {
				return fmt.Sprintf(" raw=%x", preview(b, 64))
			}
			fmt.Fprintf(&sb, " %d:varint=%d", num, v)
			rest = rest[m:]
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(rest)
			if m < 0 {
				return fmt.Sprintf(" raw=%x", preview(b, 64))
			}
			if isMostlyText(v) {
				fmt.Fprintf(&sb, " %d:bytes(%d)=%q", num, len(v), preview(v, 48))
			} else {
				fmt.Fprintf(&sb, " %d:bytes(%d)=%x", num, len(v), preview(v, 24))
			}
			rest = rest[m:]
		default:
			m := protowire.ConsumeFieldValue(num, typ, rest)
			if m < 0 {
				return fmt.Sprintf(" raw=%x", preview(b, 64))
			}
			fmt.Fprintf(&sb, " %d:wt%d(%dB)", num, typ, m)
			rest = rest[m:]
		}
	}
	return sb.String()
}

// protoTree renders a protobuf message as an indented field tree, recursing
// into length-delimited fields that themselves parse as protobuf. Leaf bytes
// are shown as text or hex. Used to inspect a captured SABR request.
func protoTree(b []byte, depth int) []string {
	indent := strings.Repeat("  ", depth+1)
	var out []string
	rest := b
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 {
			return append(out, indent+fmt.Sprintf("<unparseable: %x>", preview(b, 32)))
		}
		rest = rest[n:]
		switch typ {
		case protowire.VarintType:
			v, m := protowire.ConsumeVarint(rest)
			if m < 0 {
				return append(out, indent+"<bad varint>")
			}
			out = append(out, indent+fmt.Sprintf("field %d: varint %d", num, v))
			rest = rest[m:]
		case protowire.Fixed32Type:
			v, m := protowire.ConsumeFixed32(rest)
			if m < 0 {
				return append(out, indent+"<bad fixed32>")
			}
			out = append(out, indent+fmt.Sprintf("field %d: fixed32 %d (%g)", num, v, math.Float32frombits(v)))
			rest = rest[m:]
		case protowire.Fixed64Type:
			v, m := protowire.ConsumeFixed64(rest)
			if m < 0 {
				return append(out, indent+"<bad fixed64>")
			}
			out = append(out, indent+fmt.Sprintf("field %d: fixed64 %d", num, v))
			rest = rest[m:]
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(rest)
			if m < 0 {
				return append(out, indent+"<bad bytes>")
			}
			rest = rest[m:]
			if sub := protoTree(v, depth+1); looksLikeProto(v) && len(sub) > 0 {
				out = append(out, indent+fmt.Sprintf("field %d: message (%d bytes)", num, len(v)))
				out = append(out, sub...)
			} else if isMostlyText(v) {
				out = append(out, indent+fmt.Sprintf("field %d: bytes(%d) %q", num, len(v), preview(v, 48)))
			} else {
				out = append(out, indent+fmt.Sprintf("field %d: bytes(%d) %x", num, len(v), preview(v, 24)))
			}
		default:
			m := protowire.ConsumeFieldValue(num, typ, rest)
			if m < 0 {
				return append(out, indent+"<bad field>")
			}
			out = append(out, indent+fmt.Sprintf("field %d: wiretype %d", num, typ))
			rest = rest[m:]
		}
	}
	return out
}

// looksLikeProto reports whether b cleanly parses as a sequence of protobuf
// fields with no trailing garbage, so protoTree only recurses when sensible.
func looksLikeProto(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	rest := b
	for len(rest) > 0 {
		num, typ, n := protowire.ConsumeTag(rest)
		if n < 0 || num <= 0 {
			return false
		}
		rest = rest[n:]
		m := protowire.ConsumeFieldValue(num, typ, rest)
		if m < 0 {
			return false
		}
		rest = rest[m:]
	}
	return true
}

func preview(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func isMostlyText(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	printable := 0
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			printable++
		}
	}
	return printable*10 >= len(b)*9
}
