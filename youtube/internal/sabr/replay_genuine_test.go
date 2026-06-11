//go:build integration

// Decisive WEB-path test: replay WaxSeal's genuine (attested-/player) SABR URL +
// body from a plain Go client across several rounds, to see whether one genuine
// serverAbrStreamingUrl drives delivery PAST the ~70s preview wall (and whether
// it keeps yielding media as the body's buffered_range advances).
//
//	WAXTAP_GENUINE_FILE=~/dev/git/waxseal_genuine_request.txt \
//	  go test -tags=integration -count=1 -run TestReplayGenuineURL ./youtube/internal/sabr/ -v
package sabr

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// parseGenuine reads the genuine dump file into its URL (rn stripped) and body.
func parseGenuine(t *testing.T) (baseURL string, body []byte) {
	t.Helper()
	path := os.Getenv("WAXTAP_GENUINE_FILE")
	if path == "" {
		t.Skip("set WAXTAP_GENUINE_FILE to the genuine-request dump")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var genURL, bodyB64 string
	for _, line := range strings.Split(string(raw), "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "https://") && strings.Contains(s, "videoplayback") {
			genURL = s
		} else if s != "" && !strings.Contains(s, " ") && !strings.Contains(s, ":") && len(s) > 200 {
			bodyB64 = s
		}
	}
	if genURL == "" || bodyB64 == "" {
		t.Fatalf("could not parse URL (%d) or body (%d)", len(genURL), len(bodyB64))
	}
	body, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(bodyB64, "="))
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	u, err := url.Parse(genURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Del("rn")
	u.RawQuery = q.Encode()
	return u.String(), body
}

// topField returns the bytes of the first top-level length-delimited field num.
func topField(b []byte, num int32) []byte {
	rest := b
	for len(rest) > 0 {
		n, typ, k := protowire.ConsumeTag(rest)
		if k < 0 {
			return nil
		}
		rest = rest[k:]
		if n == protowire.Number(num) && typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(rest)
			if m < 0 {
				return nil
			}
			return v
		}
		m := protowire.ConsumeFieldValue(n, typ, rest)
		if m < 0 {
			return nil
		}
		rest = rest[m:]
	}
	return nil
}

// allTopFields returns the bytes of every top-level length-delimited field num.
func allTopFields(b []byte, num protowire.Number) [][]byte {
	var out [][]byte
	rest := b
	for len(rest) > 0 {
		n, typ, k := protowire.ConsumeTag(rest)
		if k < 0 {
			break
		}
		m := protowire.ConsumeFieldValue(n, typ, rest[k:])
		if m < 0 {
			break
		}
		if n == num && typ == protowire.BytesType {
			v, _ := protowire.ConsumeBytes(rest[k:])
			out = append(out, v)
		}
		rest = rest[k+m:]
	}
	return out
}

// topVarint returns the first top-level varint field num, or 0.
func topVarint(b []byte, num int32) uint64 {
	rest := b
	for len(rest) > 0 {
		n, typ, k := protowire.ConsumeTag(rest)
		if k < 0 {
			return 0
		}
		rest = rest[k:]
		if n == protowire.Number(num) && typ == protowire.VarintType {
			v, m := protowire.ConsumeVarint(rest)
			if m < 0 {
				return 0
			}
			return v
		}
		m := protowire.ConsumeFieldValue(n, typ, rest)
		if m < 0 {
			return 0
		}
		rest = rest[m:]
	}
	return 0
}

func TestReplayGenuineURL(t *testing.T) {
	base, body := parseGenuine(t)
	t.Logf("replaying genuine URL across rounds; body=%dB", len(body))
	client := &http.Client{Timeout: 30 * time.Second}
	for rn := 0; rn < 10; rn++ {
		ru := base + "&rn=" + strconv.Itoa(rn)
		req, _ := http.NewRequest(http.MethodPost, ru, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Accept", "application/vnd.yt-ump")
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("round %d: %v", rn, err)
		}
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, maxRoundBytes))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("round %d: HTTP %d (URL likely expired)", rn, resp.StatusCode)
		}
		mediaBytes, maxSeq, status := summarize(t, rb)
		t.Logf("round %d (rn=%d): %d resp bytes, media=%dB, maxSeq=%d, status=%d",
			rn, rn, len(rb), mediaBytes, maxSeq, status)
	}
}

// summarize parses a UMP body and returns total media bytes, the highest media
// header sequence number seen, and the last STREAM_PROTECTION_STATUS.
func summarize(t *testing.T, body []byte) (mediaBytes int, maxSeq uint64, status int32) {
	r := newUMPReader(body)
	headers := map[uint32]uint64{}
	for {
		part, ok, err := r.next()
		if err != nil {
			t.Logf("  ump error: %v", err)
			return
		}
		if !ok {
			return
		}
		switch part.Type {
		case partMediaHeader:
			h, err := unmarshalMediaHeader(part.Payload)
			if err == nil {
				headers[h.HeaderID] = h.SequenceNumber
				if h.SequenceNumber > maxSeq {
					maxSeq = h.SequenceNumber
				}
			}
		case partMedia:
			id, media, err := leadingVarint(part.Payload)
			if err == nil {
				mediaBytes += len(media)
				_ = id
			}
		case partStreamProtection:
			st, err := unmarshalStreamProtectionStatus(part.Payload)
			if err == nil {
				status = st.Status
			}
		}
	}
}

// TestStreamGenuineURL drives our ACTUAL SABR client (Open) against the genuine
// attested-/player URL, advancing buffered_range round to round, to confirm one
// genuine URL streams full audio from our Go code: the proposed fix is a
// pure URL handoff with no other client change.
func TestStreamGenuineURL(t *testing.T) {
	base, body := parseGenuine(t)
	ustreamer := topField(body, int32(fAbrUstreamerConfig))
	if ustreamer == nil {
		t.Fatal("could not extract ustreamer config (field 5) from genuine body")
	}
	// Pull the genuine session's streamerContext so our body coheres with the
	// session the URL was signed for (a minimal body triggers RELOAD_PLAYER).
	sctx := topField(body, int32(fAbrStreamerContext))
	pot := topField(sctx, int32(fStreamerCtxPOToken))
	ci := topField(sctx, int32(fStreamerCtxClientInfo))
	clientInfo := ClientInfo{
		ClientName:     int32(topVarint(ci, int32(fClientInfoClientName))),
		ClientVersion:  string(topField(ci, int32(fClientInfoClientVersion))),
		OSName:         string(topField(ci, int32(fClientInfoOSName))),
		OSVersion:      string(topField(ci, int32(fClientInfoOSVersion))),
		DeviceMake:     string(topField(ci, int32(fClientInfoDeviceMake))),
		DeviceModel:    string(topField(ci, int32(fClientInfoDeviceModel))),
		AcceptLanguage: string(topField(ci, int32(fClientInfoAcceptLanguage))),
	}
	// Use a format id coherent with this session: the first preferred_audio entry
	// (field 16). A bare (itag, lmt) with the wrong/absent xtags matches no format
	// and the server answers RELOAD_PLAYER_RESPONSE; the real path is always
	// coherent because sabrFormatID copies itag+lmt+xtags from the /player format.
	audio := allTopFields(body, fAbrPreferredAudioFormats)
	if len(audio) == 0 {
		t.Fatal("no preferred_audio_format_ids (field 16) in genuine body")
	}
	format, err := unmarshalFormatId(audio[0])
	if err != nil {
		t.Fatalf("decode preferred_audio[0]: %v", err)
	}
	t.Logf("genuine URL stream test: ustreamer=%dB pot=%dB client_name=%d ver=%q format=itag %d lmt %d xtags %q",
		len(ustreamer), len(pot), clientInfo.ClientName, clientInfo.ClientVersion, format.Itag, format.LastModified, format.XTags)

	cfg := Config{
		HTTP:            http.DefaultClient,
		ServerAbrURL:    base,
		UstreamerConfig: ustreamer,
		Format:          format,
		ClientInfo:      clientInfo,
		POToken:         pot,
		ContentLength:   0,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rc, info, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	t.Logf("streamed %d bytes (content-type %q) err=%v", n, info.ContentType, err)
	// itag 251 (Opus 128k) full file is ~9.7 MiB; the 70s preview is ~0.96 MB.
	if n < 5<<20 {
		t.Fatalf("only %d bytes, looks capped, not a full stream", n)
	}
	t.Logf("SUCCESS: genuine URL streamed %d bytes (>5 MiB), past the preview wall", n)
}

// TestStreamPlayerContext points our production cold-start SABR client at a FRESH
// pre-stream attested /player context (serverAbrStreamingUrl rn=0 + ustreamer),
// to confirm the proposed WEB fix: a /player-context handoff lets the existing Go
// loop stream full audio at status 1.
//
//	WAXTAP_PLAYER_CONTEXT=~/dev/git/waxseal_player_context.txt \
//	  go test -tags=integration -count=1 -run TestStreamPlayerContext ./youtube/internal/sabr/ -v
func TestStreamPlayerContext(t *testing.T) {
	path := os.Getenv("WAXTAP_PLAYER_CONTEXT")
	if path == "" {
		t.Skip("set WAXTAP_PLAYER_CONTEXT to the player-context dump")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	var serverURL, ustreamerB64 string
	for i, line := range lines {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "https://") && strings.Contains(s, "videoplayback") {
			serverURL = s
		}
		if strings.Contains(s, "videoPlaybackUstreamerConfig") {
			for j := i + 1; j < len(lines); j++ {
				if v := strings.TrimSpace(lines[j]); v != "" {
					ustreamerB64 = v
					break
				}
			}
		}
	}
	if serverURL == "" || ustreamerB64 == "" {
		t.Fatalf("parse failed: url=%d ustreamer=%d", len(serverURL), len(ustreamerB64))
	}
	ustreamer, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(ustreamerB64, "="))
	if err != nil {
		t.Fatalf("decode ustreamer: %v", err)
	}
	t.Logf("cold-start against fresh /player context: ustreamer=%dB url=%dchars", len(ustreamer), len(serverURL))

	cfg := Config{
		HTTP:            http.DefaultClient,
		ServerAbrURL:    serverURL,
		UstreamerConfig: ustreamer,
		Format:          FormatId{Itag: 251, LastModified: 1719185037039937},
		ClientInfo:      ClientInfo{ClientName: 1, ClientVersion: "2.20260606.02.00"},
		UserAgent:       "Mozilla/5.0 (X11; Ubuntu; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	rc, info, err := Open(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, rc)
	t.Logf("streamed %d bytes (content-type %q) err=%v", n, info.ContentType, err)
	if n < 5<<20 {
		t.Fatalf("only %d bytes, capped, not full (itag 251 full ~9.7 MiB)", n)
	}
	t.Logf("SUCCESS: cold-start client streamed %d bytes from the /player context: WEB fix works", n)
}
