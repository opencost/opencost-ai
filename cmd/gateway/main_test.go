package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opencost/opencost-ai/pkg/apiv1"
)

func TestHealthHandler_ReturnsOK(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newMux(slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}
	var body apiv1.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status field = %q, want ok", body.Status)
	}
}

func TestHealthHandler_RejectsWrongMethod(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newMux(slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/health", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/health: %v", err)
	}
	defer resp.Body.Close()
	// Go 1.22 ServeMux returns 405 for method-mismatched routes when
	// a handler exists for the same pattern under a different method.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestHealthHandler_UnknownPath404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newMux(slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
