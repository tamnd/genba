package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestVersionPrintsAndExits(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(t.Context(), []string{"-version"}, env(nil), &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(out.String(), "genbad ") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestUnknownStoreIsReported(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-store", "cassandra", "-dsn", "x"}, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("run accepted an unknown store")
	}
	if !strings.Contains(err.Error(), "cassandra") {
		t.Fatalf("error %q does not name the store", err)
	}
}

func TestServerServesAndShutsDown(t *testing.T) {
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, []string{"-addr", addr, "-log-level", "error"}, env(nil), &out, &errOut)
	}()

	waitForHealth(t, "http://"+addr+"/healthz")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not shut down after its context was cancelled")
	}
}

// TestMetricsAreOnTheirOwnAddress is the deployment shape the whole second
// listener exists for. The numbers are published where an operator can scrape
// them and nowhere the API is reachable from.
func TestMetricsAreOnTheirOwnAddress(t *testing.T) {
	addr, metricsAddr := freeAddr(t), freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, []string{"-addr", addr, "-metrics-addr", metricsAddr, "-log-level", "error"}, env(nil), &out, &errOut)
	}()

	waitForHealth(t, "http://"+addr+"/healthz")

	if body := get(t, "http://"+metricsAddr+"/metrics"); !strings.Contains(body, "genba_request_duration_milliseconds") {
		t.Errorf("the metrics listener served:\n%s", body)
	}
	if body := get(t, "http://"+addr+"/metrics"); strings.Contains(body, "genba_request_duration_milliseconds") {
		t.Error("the API address is serving metrics")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not shut down after its context was cancelled")
	}
}

// TestMetricsAreOffByDefault, because a port that opens itself is a port
// somebody has to find out about from a scan.
func TestMetricsAreOffByDefault(t *testing.T) {
	addr, metricsAddr := freeAddr(t), freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		var out, errOut bytes.Buffer
		_ = run(ctx, []string{"-addr", addr, "-log-level", "error"}, env(nil), &out, &errOut)
	}()

	waitForHealth(t, "http://"+addr+"/healthz")

	conn, err := net.DialTimeout("tcp", metricsAddr, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Errorf("something is listening on %s with no metrics address configured", metricsAddr)
	}
}

// get reads a URL, returning an empty body for a connection that is refused or
// a response that is not a 200, because both mean the same thing here.
func get(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return string(body)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("releasing the port: %v", err)
	}
	return addr
}

func waitForHealth(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
		if err != nil {
			t.Fatalf("building the request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the server never became healthy")
}
