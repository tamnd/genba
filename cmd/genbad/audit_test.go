package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/audit"
)

// TestASearchOnARunningServerIsOnTheTrail is the whole feature seen from
// outside.
//
// Everything below this is a unit test of a part of it. This one starts the
// binary the way a unit file does, asks it a question the way a person does,
// stops it the way a deployment does, and then reads the directory the way
// somebody answering a question about last quarter does. If any of the wiring
// is missing, the directory is empty and this is the test that says so.
func TestASearchOnARunningServerIsOnTheTrail(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, []string{
			"-addr", addr,
			"-tenant", "acme",
			"-audit-dir", dir,
			"-log-level", "error",
		}, env(nil), &out, &errOut)
	}()

	waitForHealth(t, "http://"+addr+"/healthz")
	askAs(t, "http://"+addr+"/api/v1/search?q=payments", "u_mei")

	// The records are written behind the request, so the trail is read after the
	// process has stopped rather than racing it. That is also the shape of the
	// promise: a shutdown does not lose what was already served.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not shut down after its context was cancelled")
	}

	var got []audit.Record
	err := audit.Records(dir, time.Time{}, time.Now().Add(time.Hour), func(rec audit.Record) error {
		got = append(got, rec)
		return nil
	})
	if err != nil {
		t.Fatalf("reading the trail: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the server served a search and left no record of it")
	}
	found := false
	for _, rec := range got {
		if rec.Action == audit.Search && rec.Subject == "u_mei" && rec.Tenant == "acme" && rec.Query == "payments" {
			found = true
		}
	}
	if !found {
		t.Errorf("the search is not on the trail: %+v", got)
	}
}

// TestADirectoryThatCannotBeOpenedStopsTheProcess rather than being worked
// around.
//
// A server that came up anyway and wrote to the log instead would be serving
// the same content under a promise it is not keeping, and the only sign of it
// would be a line in a log nobody looks at until they need the records.
func TestADirectoryThatCannotBeOpenedStopsTheProcess(t *testing.T) {
	// A file where the directory was meant to be, which is what a bad path in a
	// unit file usually turns out to be.
	blocked := filepath.Join(t.TempDir(), "audit")
	if err := os.WriteFile(blocked, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("writing the blocking file: %v", err)
	}

	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-addr", freeAddr(t), "-audit-dir", blocked}, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("the server started with nowhere to write its audit trail")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Fatalf("error %q does not say what could not be opened", err)
	}
}

// TestARetentionWithNowhereToApplyItIsRefused. Setting one and no directory is
// somebody believing they have said how long their trail is kept while the
// records go to the process log and this setting deletes nothing.
func TestARetentionWithNowhereToApplyItIsRefused(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-addr", freeAddr(t), "-audit-retention", "720h"}, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("the server accepted a retention with no files to delete")
	}
	if !strings.Contains(err.Error(), "retention") {
		t.Fatalf("error %q does not mention the retention", err)
	}
}

// TestTheTrailIsWrittenToTheLogWhenNoDirectoryIsConfigured, because the default
// is a different destination and not a missing one. This is the laptop case and
// the collector case, and it is still a trail.
func TestTheTrailIsWrittenToTheLogWhenNoDirectoryIsConfigured(t *testing.T) {
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	// Read after run has returned, which is after every deferred close has run,
	// so nothing is still writing to it while this reads.
	var out, errOut bytes.Buffer
	go func() {
		done <- run(ctx, []string{"-addr", addr, "-tenant", "acme"}, env(nil), &out, &errOut)
	}()

	waitForHealth(t, "http://"+addr+"/healthz")
	askAs(t, "http://"+addr+"/api/v1/search?q=payments", "u_mei")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not shut down after its context was cancelled")
	}

	logged := errOut.String()
	if !strings.Contains(logged, audit.Message) || !strings.Contains(logged, "u_mei") {
		t.Errorf("the search is not in the log:\n%s", logged)
	}
}

// askAs makes one request as somebody, and fails on anything but a 200 so that
// a test about records is never quietly a test about a 404.
func askAs(t *testing.T, url, subject string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set(api.HeaderSubject, subject)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("asking %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s answered %d", url, resp.StatusCode)
	}
}
