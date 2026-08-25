package main

import (
	"bytes"
	"context"
	"encoding/json"
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

	// The health check is on the API address, and the two listeners bind
	// independently, so a healthy API says nothing about whether the metrics
	// port is open yet. Asking once and reading a refused connection as an empty
	// body made this a test of how busy the machine is.
	if body := waitForMetrics(t, "http://"+metricsAddr+"/metrics"); !strings.Contains(body, "genba_request_duration_milliseconds") {
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

// waitForMetrics reads the metrics endpoint once it is up, and returns whatever
// it read last when it never is, so the caller reports what it got rather than
// a timeout with nothing in it.
func waitForMetrics(t *testing.T, url string) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		body := get(t, url)
		if body != "" || !time.Now().Before(deadline) {
			return body
		}
		time.Sleep(20 * time.Millisecond)
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

// waitForIndex waits until the server has finished reading its sources for the
// first time.
//
// A test that asserts on what is in the index needs this and not the health
// check. The server answers from the moment it binds, and everything a
// connector is doing it is doing behind that, so a query sent the microsecond
// the port opens is a query against an index that is legitimately empty.
func waitForIndex(t *testing.T, base string) {
	t.Helper()
	waitForHealth(t, base+"/healthz")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var ready struct {
			Indexing bool `json:"indexing"`
		}
		body := get(t, base+"/readyz")
		if body != "" && json.Unmarshal([]byte(body), &ready) == nil && !ready.Indexing {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the server was still indexing after thirty seconds")
}

// waitForHealth waits for the listener to answer.
//
// The deadline is long because the interface is compressed before the listener
// binds, once per process, and both encoders are asked for their best. That is
// a fraction of a second in an ordinary build and it is over a minute in a race
// build on a small machine, so a shorter deadline here fails on the slowest
// machine anybody runs the suite on rather than on a server that is broken.
func waitForHealth(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
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
