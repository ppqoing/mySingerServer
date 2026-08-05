package stats

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPprofMuxServesIndexWithoutDefaultMux(t *testing.T) {
	server := httptest.NewServer(newPprofMux())
	defer server.Close()
	response, err := http.Get(server.URL + "/debug/pprof/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(response.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("pprof response status/content-type = %d/%q",
			response.StatusCode, response.Header.Get("Content-Type"))
	}
}

func TestStartPprofRejectsNonLoopbackAddress(t *testing.T) {
	err := StartPprof(
		context.Background(),
		"0.0.0.0:0",
		slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	)
	if err == nil {
		t.Fatal("StartPprof accepted non-loopback address")
	}
}
