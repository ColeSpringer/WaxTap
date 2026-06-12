package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sidecarJSON is the shared wire plumbing of the CLI's sidecar providers
// (bgutil PO-token, session, player-context): JSON in (optional) and out over
// a dedicated HTTP client, with a short body snippet on non-200 responses to
// aid diagnosis. label names the sidecar in errors (e.g. "session endpoint").
func sidecarJSON(ctx context.Context, client *http.Client, method, endpoint, label string, in, out any) error {
	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// Preserve the provider name and endpoint so the CLI can distinguish a
		// connection failure from the sentinel that may wrap it.
		return &sidecarError{label: label, endpoint: endpoint, err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%s returned %s: %s", label, resp.Status, strings.TrimSpace(string(snippet)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", label, err)
	}
	return nil
}

// sidecarError reports a connection failure to a configured provider endpoint.
// It remains available through wrappers such as ErrNeedsPOToken.
type sidecarError struct {
	label    string
	endpoint string
	err      error
}

func (e *sidecarError) Error() string {
	return fmt.Sprintf("%s unreachable at %s: %v", e.label, e.endpoint, e.err)
}

func (e *sidecarError) Unwrap() error { return e.err }
