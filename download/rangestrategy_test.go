package download

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/colespringer/waxtap/waxerr"
)

func TestHeaderRange_Apply(t *testing.T) {
	tests := []struct {
		start, end int64
		want       string
	}{
		{0, 99, "bytes=0-99"},
		{100, 199, "bytes=100-199"},
		{4000, -1, "bytes=4000-"},
	}
	for _, tt := range tests {
		req, _ := http.NewRequest(http.MethodGet, "https://x/y", nil)
		HeaderRange{}.Apply(req, tt.start, tt.end)
		if got := req.Header.Get("Range"); got != tt.want {
			t.Errorf("Apply(%d,%d) Range = %q, want %q", tt.start, tt.end, got, tt.want)
		}
	}
}

func TestQueryRange_Apply(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://x/y?z=1", nil)
	QueryRange{}.Apply(req, 100, 199)
	if got := req.URL.Query().Get("range"); got != "100-199" {
		t.Errorf("range query = %q, want 100-199", got)
	}
	if got := req.URL.Query().Get("z"); got != "1" {
		t.Errorf("existing query lost: z = %q, want 1", got)
	}

	req2, _ := http.NewRequest(http.MethodGet, "https://x/y", nil)
	QueryRange{}.Apply(req2, 4000, -1)
	if got := req2.URL.Query().Get("range"); got != "4000-" {
		t.Errorf("open-ended range query = %q, want 4000-", got)
	}
}

func resp(status int, contentLength int64, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, ContentLength: contentLength, Header: header}
}

func TestHeaderRange_Validate(t *testing.T) {
	tests := []struct {
		name             string
		resp             *http.Response
		start, end       int64
		ranged           bool
		wantErr          bool
		wantStatusError  bool
		wantIgnoredRange bool
	}{
		{"ranged 206 correct length", resp(206, 90, nil), 10, 99, true, false, false, false},
		{"ranged 206 wrong length", resp(206, 50, nil), 10, 99, true, true, false, false},
		{"ranged 206 unknown length", resp(206, -1, nil), 10, 99, true, false, false, false},
		{"ranged 200 ignored range", resp(200, 100, nil), 10, 99, true, true, false, true},
		{"ranged 404", resp(404, 0, nil), 10, 99, true, true, true, false},
		{"unranged 200", resp(200, 100, nil), 0, -1, false, false, false, false},
		{"unranged 500", resp(500, 0, nil), 0, -1, false, true, true, false},
		{"open-ended 206", resp(206, -1, nil), 4000, -1, true, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := HeaderRange{}.Validate(tt.resp, tt.start, tt.end, tt.ranged)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantStatusError {
				var se *waxerr.HTTPStatusError
				if !errors.As(err, &se) {
					t.Fatalf("err = %v, want *waxerr.HTTPStatusError", err)
				}
			}
			if tt.wantIgnoredRange && (err == nil || !strings.Contains(err.Error(), "ignored Range")) {
				t.Fatalf("err = %v, want ignored-Range error", err)
			}
		})
	}
}

func TestQueryRange_Validate(t *testing.T) {
	// googlevideo answers a &range= query with 200, not 206.
	if err := (QueryRange{}).Validate(resp(200, 90, nil), 10, 99, true); err != nil {
		t.Errorf("200 ranged: unexpected error %v", err)
	}
	if err := (QueryRange{}).Validate(resp(206, 90, nil), 10, 99, true); err == nil {
		t.Errorf("206 should be rejected by QueryRange (expects 200)")
	}
	if err := (QueryRange{}).Validate(resp(200, 50, nil), 10, 99, true); err == nil {
		t.Errorf("wrong length should error")
	}
}

func TestContentRangeTotal(t *testing.T) {
	tests := []struct {
		header string
		want   int64
	}{
		{"bytes 0-99/200", 200},
		{"bytes 100-199/200", 200},
		{"bytes 0-99/*", 0},
		{"", 0},
		{"garbage", 0},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.header != "" {
			h.Set("Content-Range", tt.header)
		}
		if got := contentRangeTotal(resp(206, -1, h)); got != tt.want {
			t.Errorf("contentRangeTotal(%q) = %d, want %d", tt.header, got, tt.want)
		}
	}
}

func TestHeaderRange_ValidateContentRangeMismatch(t *testing.T) {
	mk := func(cr string, cl int64) *http.Response {
		h := http.Header{}
		h.Set("Content-Range", cr)
		return resp(http.StatusPartialContent, cl, h)
	}
	// A matching length is not enough if the bytes belong to a different offset.
	if err := (HeaderRange{}).Validate(mk("bytes 0-999/8000", 1000), 1000, 1999, true); err == nil {
		t.Error("expected error for Content-Range starting at the wrong offset")
	}
	// Wrong end.
	if err := (HeaderRange{}).Validate(mk("bytes 1000-1998/8000", 999), 1000, 1999, true); err == nil {
		t.Error("expected error for Content-Range ending at the wrong offset")
	}
	// Correct Content-Range passes.
	if err := (HeaderRange{}).Validate(mk("bytes 1000-1999/8000", 1000), 1000, 1999, true); err != nil {
		t.Errorf("correct Content-Range: unexpected error %v", err)
	}
	// Open-ended resume: only the start offset is verified.
	if err := (HeaderRange{}).Validate(mk("bytes 4000-7999/8000", -1), 4000, -1, true); err != nil {
		t.Errorf("open-ended resume: unexpected error %v", err)
	}
	if err := (HeaderRange{}).Validate(mk("bytes 3999-7999/8000", -1), 4000, -1, true); err == nil {
		t.Error("expected error for open-ended resume starting at the wrong offset")
	}
}

func TestParseContentRange(t *testing.T) {
	tests := []struct {
		header                        string
		wantStart, wantEnd, wantTotal int64
		wantOK                        bool
	}{
		{"bytes 0-999/8000", 0, 999, 8000, true},
		{"bytes 1000-1999/8000", 1000, 1999, 8000, true},
		{"bytes 0-99/*", 0, 99, 0, true},
		{"bytes */8000", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"garbage", 0, 0, 0, false},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.header != "" {
			h.Set("Content-Range", tt.header)
		}
		start, end, total, ok := parseContentRange(resp(http.StatusPartialContent, -1, h))
		if ok != tt.wantOK || (ok && (start != tt.wantStart || end != tt.wantEnd || total != tt.wantTotal)) {
			t.Errorf("parseContentRange(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				tt.header, start, end, total, ok, tt.wantStart, tt.wantEnd, tt.wantTotal, tt.wantOK)
		}
	}
}
