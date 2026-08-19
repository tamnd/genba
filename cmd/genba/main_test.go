package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

// fakeServer answers the two endpoints this client calls, and records the
// credential headers so that the test can check they were forwarded.
func fakeServer(t *testing.T, seen *http.Header) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"status":"ok","version":"test","uptime":"1s"}`))
	})
	mux.HandleFunc("GET /api/v1/search", func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Clone()
		if r.Header.Get("X-Genba-Subject") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"total":1,"hits":[{"id":"d1","title":"Payments runbook","source":"gdrive","url":"https://example.com/d1"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSearchPrintsResults(t *testing.T) {
	var seen http.Header
	base := fakeServer(t, &seen)

	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"search", "payments", "runbook"}, env(map[string]string{
		"GENBA_SERVER":  base,
		"GENBA_SUBJECT": "u_mei",
		"GENBA_GROUPS":  "gdrive:eng@acme.com",
	}), &out, &errOut)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "Payments runbook") {
		t.Fatalf("output = %q", out.String())
	}
	if seen.Get("X-Genba-Subject") != "u_mei" || seen.Get("X-Genba-Groups") == "" {
		t.Fatalf("the client did not forward its credentials: %v", seen)
	}
}

func TestSearchWithoutCredentialsSaysWhatToSet(t *testing.T) {
	var seen http.Header
	base := fakeServer(t, &seen)

	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"search", "payments"}, env(map[string]string{"GENBA_SERVER": base}), &out, &errOut)
	if err == nil {
		t.Fatal("run succeeded without a credential")
	}
	if !strings.Contains(err.Error(), "GENBA_SUBJECT") {
		t.Fatalf("error %q does not say what to set", err)
	}
}

func TestSearchNeedsAQuery(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(t.Context(), []string{"search"}, env(nil), &out, &errOut); err == nil {
		t.Fatal("run accepted an empty query")
	}
}

func TestHealth(t *testing.T) {
	var seen http.Header
	base := fakeServer(t, &seen)

	var out, errOut bytes.Buffer
	if err := run(t.Context(), []string{"health"}, env(map[string]string{"GENBA_SERVER": base}), &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(t.Context(), []string{"reindex"}, env(nil), &out, &errOut); err == nil {
		t.Fatal("run accepted an unknown command")
	}
	if !strings.Contains(errOut.String(), "Usage") {
		t.Fatal("an unknown command did not print the usage")
	}
}

func TestNoCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(t.Context(), nil, env(nil), &out, &errOut); err == nil {
		t.Fatal("run accepted no command")
	}
}
