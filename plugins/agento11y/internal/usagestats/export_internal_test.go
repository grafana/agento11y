package usagestats

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestExportPostsTheEventVerbatim pins the request shape.
func TestExportPostsTheEventVerbatim(t *testing.T) {
	type captured struct {
		method      string
		path        string
		contentType string
		userAgent   string
		body        []byte
	}
	got := make(chan captured, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- captured{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			userAgent:   r.Header.Get("User-Agent"),
			body:        body,
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	want := fullyPopulatedEvent()
	export(want, server.URL+"/agento11y-usage-report", "1.2.3")

	req := <-got
	if req.method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.method)
	}
	if req.path != "/agento11y-usage-report" {
		t.Errorf("path = %q", req.path)
	}
	if req.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.contentType)
	}
	if req.userAgent != "agento11y-usage-stats/1.2.3" {
		t.Errorf("User-Agent = %q; usage-stats traffic must be distinguishable from generation export", req.userAgent)
	}

	var decoded Event
	if err := json.Unmarshal(req.body, &decoded); err != nil {
		t.Fatalf("body is not an Event: %v", err)
	}
	if decoded != want {
		t.Errorf("body round-trip = %+v\nwant %+v", decoded, want)
	}
}

// TestExportCarriesNoTimestamp: the server stamps receipt time, so a wrong
// client clock cannot skew it.
func TestExportCarriesNoTimestamp(t *testing.T) {
	body, err := json.Marshal(fullyPopulatedEvent())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"ts", "timestamp", "time", "created_at", "recorded_at"} {
		if _, present := decoded[key]; present {
			t.Errorf("payload carries %q; the receiver stamps receipt time", key)
		}
	}
}

// TestExportIsSilentOnFailure: an unreachable or hostile endpoint must not
// panic, block, or affect the exiting command.
func TestExportIsSilentOnFailure(t *testing.T) {
	t.Run("connection refused", func(t *testing.T) {
		export(fullyPopulatedEvent(), "http://127.0.0.1:0/nope", "1.2.3")
	})

	t.Run("server error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()
		export(fullyPopulatedEvent(), server.URL, "1.2.3")
	})

	t.Run("unparseable url", func(t *testing.T) {
		export(fullyPopulatedEvent(), "://not a url", "1.2.3")
	})
}

// TestEndpointOverride pins the dev-build escape hatch.
func TestEndpointOverride(t *testing.T) {
	t.Setenv(EnvEndpoint, "http://localhost:9999/report")
	if got := endpoint(); got != "http://localhost:9999/report" {
		t.Errorf("endpoint() = %q, want the override", got)
	}

	t.Setenv(EnvEndpoint, "   ")
	if got := endpoint(); got != DefaultEndpoint {
		t.Errorf("endpoint() = %q, want %q for a blank override", got, DefaultEndpoint)
	}
}

func TestUserAgentFallsBackForUnstampedBuilds(t *testing.T) {
	if got := UserAgent(""); got != "agento11y-usage-stats/dev" {
		t.Errorf("UserAgent(%q) = %q", "", got)
	}
}
