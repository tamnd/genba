package main

import (
	"bytes"
	"context"
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
