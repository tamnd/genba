package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The first ninety seconds, end to end.
//
// A server pointed at a corpus it has never read used to bind its port after
// the walk finished. On a real tree that is a minute and a half of connection
// refused, which a readiness probe reads as a failed deploy and a person reads
// as a hang. It answers from the first second now, and the price of that is
// that the first answers come out of an index that is still filling. So it says
// so, on every screen, until it is done.

// bigTree writes enough files that reading them takes long enough to watch. The
// count is not arbitrary: the whole point of the test is to look at the server
// while a first read is in flight, and a tree of eight files is read faster than
// a request can be sent.
func bigTree(t *testing.T, files int) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range files {
		name := filepath.Join(root, "notes", fmt.Sprintf("note-%04d.md", i))
		body := fmt.Sprintf("# Note %04d\n\nOne paragraph about deploying, written so the file is worth reading and parsing.\n", i)
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// startCorpus starts a server on a directory and returns its address and a
// function that shuts it down. It deliberately does not wait for the index.
func startCorpus(t *testing.T, root string) (addr string, stop func()) {
	t.Helper()
	addr = freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, []string{
			"-addr", addr,
			"-tenant", "acme",
			"-corpus", root,
			"-corpus-name", "handbook",
			"-log-level", "error",
		}, env(nil), &out, &errOut)
	}()
	return addr, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("the server did not shut down")
		}
	}
}

func TestTheServerAnswersBeforeTheCorpusIsReadAndSaysSo(t *testing.T) {
	addr, stop := startCorpus(t, bigTree(t, 2000))
	defer stop()

	// This is the assertion. The health check answering at all, on a corpus that
	// cannot have been read yet, is the behaviour the whole change is for.
	waitForHealth(t, "http://"+addr+"/healthz")

	var ready struct {
		Status   string `json:"status"`
		Indexing bool   `json:"indexing"`
	}
	body := get(t, "http://"+addr+"/readyz")
	if body == "" {
		t.Fatal("the readiness check did not answer on a server that is already healthy")
	}
	if err := json.Unmarshal([]byte(body), &ready); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if ready.Status != "ready" {
		t.Errorf("status is %q, want a server that is reading its corpus to still be ready", ready.Status)
	}
	if !ready.Indexing {
		t.Fatal("a server that has not finished its first read says nothing is being indexed")
	}

	// A query is answered rather than refused or held, out of whatever has been
	// written so far. Nothing is asserted about the number of results, because
	// how many there are at this instant is a race by design.
	res := searchAs(t, addr, "alice", "deploying")
	if res.Total < 0 {
		t.Errorf("search returned %+v", res)
	}

	// And an authenticated caller gets the numbers, which is what the interface
	// puts in the banner.
	stats := statsAs(t, addr, "alice")
	if stats.Indexing == nil {
		t.Fatal("stats said nothing about the read that is running")
	}
	if stats.Indexing.Source != "handbook" {
		t.Errorf("indexing names %q, want the source from -corpus-name", stats.Indexing.Source)
	}
	if stats.Indexing.Done > stats.Indexing.Total && stats.Indexing.Total != 0 {
		t.Errorf("indexing = %+v, more read than there is to read", *stats.Indexing)
	}
}

// And it stops saying it. A banner that is still up after the corpus is read is
// worse than never having had one, because the next time it is true nobody
// reads it.
func TestNothingIsReportedOnceTheCorpusIsRead(t *testing.T) {
	addr, stop := startCorpus(t, corpusTree(t))
	defer stop()

	waitForIndex(t, "http://"+addr)

	if body := get(t, "http://"+addr+"/readyz"); strings.Contains(body, "indexing") {
		t.Errorf("readyz still mentions indexing on a corpus that is read: %s", body)
	}
	if got := statsAs(t, addr, "alice").Indexing; got != nil {
		t.Errorf("stats still reports %+v on a corpus that is read", *got)
	}

	// The results are all there, which is the thing the banner was promising
	// would arrive.
	if res := searchAs(t, addr, "alice", "deploying"); res.Total == 0 {
		t.Error("the first read finished and the corpus is not searchable")
	}
}

type statsResult struct {
	Documents int `json:"documents"`
	Indexing  *struct {
		Source string `json:"source"`
		Done   int    `json:"done"`
		Total  int    `json:"total"`
	} `json:"indexing"`
}

func statsAs(t *testing.T, addr, subject string) statsResult {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/api/v1/stats", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Genba-Subject", subject)
	req.Header.Set("X-Genba-Tenant", "acme")
	req.Header.Set("X-Genba-Identities", "github:"+subject)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stats returned %s", resp.Status)
	}
	var out statsResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	return out
}
