package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxtap/potoken"
)

// bgutilProvider is a potoken.Provider that mints PO tokens from a bgutil-wire
// HTTP server (the protocol bgutil-ytdlp-pot-provider and WaxSeal's token server
// speak). It POSTs {"content_binding": …} to <baseURL>/get_pot and maps the
// returned poToken/expiresAt onto a potoken.Response.
//
// The content binding is scope-specific: a player-scope token binds to the video
// ID; a GVS (stream) token binds to the session's visitor-data string.
//
// Token traffic uses a dedicated HTTP client, never WaxTap's --proxy/--insecure
// client: the provider is typically a localhost sidecar that must not be proxied,
// and full byte-level WEB validation only works when the token request and the
// stream egress the same IP (the token is bound to the minting host).
type bgutilProvider struct {
	endpoint string
	http     *http.Client
}

// newBgutilProvider builds a provider that talks to the bgutil server at baseURL
// (e.g. "http://127.0.0.1:4417").
func newBgutilProvider(baseURL string) *bgutilProvider {
	return &bgutilProvider{
		endpoint: strings.TrimRight(baseURL, "/") + "/get_pot",
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

// bgutilRequest and bgutilResponse mirror the bgutil /get_pot wire contract.
type bgutilRequest struct {
	ContentBinding string `json:"content_binding"`
}

type bgutilResponse struct {
	POToken        string `json:"poToken"`
	ContentBinding string `json:"contentBinding"`
	ExpiresAt      string `json:"expiresAt"`
}

func (p *bgutilProvider) ProvidePOToken(ctx context.Context, req potoken.Request) (potoken.Response, error) {
	binding, err := contentBinding(req)
	if err != nil {
		return potoken.Response{}, err
	}
	body, err := json.Marshal(bgutilRequest{ContentBinding: binding})
	if err != nil {
		return potoken.Response{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return potoken.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := p.http.Do(httpReq)
	if err != nil {
		return potoken.Response{}, fmt.Errorf("bgutil PO-token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Surface a short snippet of the body to aid diagnosis.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return potoken.Response{}, fmt.Errorf("bgutil PO-token server returned %s: %s",
			resp.Status, strings.TrimSpace(string(snippet)))
	}

	var out bgutilResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return potoken.Response{}, fmt.Errorf("decode bgutil PO-token response: %w", err)
	}
	if out.POToken == "" {
		return potoken.Response{}, fmt.Errorf("bgutil PO-token server returned an empty token")
	}
	return potoken.Response{
		Token:     out.POToken,
		ExpiresAt: parseBgutilExpiry(out.ExpiresAt),
	}, nil
}

// contentBinding selects the bgutil content_binding for the token scope: a
// player token binds to the video ID; a GVS (stream) token binds to the raw
// visitor-data string. Other scopes are unsupported by this provider.
func contentBinding(req potoken.Request) (string, error) {
	switch req.Scope {
	case potoken.ScopePlayer:
		if req.VideoID == "" {
			return "", fmt.Errorf("bgutil: player PO token requested without a video ID")
		}
		return req.VideoID, nil
	case potoken.ScopeGVS:
		if req.VisitorData == "" {
			return "", fmt.Errorf("bgutil: GVS PO token requested without visitor data")
		}
		return req.VisitorData, nil
	default:
		return "", fmt.Errorf("bgutil: unsupported PO-token scope %q", req.Scope)
	}
}

// parseBgutilExpiry reads the bgutil expiresAt field, normally RFC3339
// ("2026-06-09T07:25:25Z") but tolerated as Unix seconds. An empty or
// unparseable value yields the zero time, which the caller treats as unknown.
func parseBgutilExpiry(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(secs, 0).UTC()
	}
	return time.Time{}
}
