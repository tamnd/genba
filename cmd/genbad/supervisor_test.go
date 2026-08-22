package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
)

// Connectors added without restarting the server.
//
// The happy path is the least interesting half of this. What is worth testing
// hardest is what happens to a corpus when a connector is removed, what happens
// to a connector when the process comes back, and what happens when somebody
// tries to change one that came off the command line. Those are the three ways
// a feature like this loses somebody's index.

// newTestSupervisor builds one over the reference driver, which remembers
// connectors for exactly as long as the test does.
func newTestSupervisor(t *testing.T) (*supervisor, *memstore.Store, *operations) {
	t.Helper()
	st := newTestStore(t)
	ops := newOperations()
	sup := newSupervisor(st, st, "acme", credentials{}, newIndexing(t.Context(), st), ops, quietLog())
	t.Cleanup(sup.stop)
	return sup, st, ops
}

func newTestStore(t *testing.T) *memstore.Store {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// corpusFeedFor is a directory connector over a tree, as the interface sends
// one.
func corpusFeedFor(t *testing.T, source, dir string, enabled bool) store.Feed {
	t.Helper()
	return store.Feed{
		Tenant:  "acme",
		Source:  source,
		Kind:    kindCorpus,
		Enabled: enabled,
		Config:  settings(t, corpusConfig{Dir: dir, ACL: aclTenant}),
		By:      "u_ops",
	}
}

// settings is a connector's settings as they cross the interface.
func settings(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// indexed waits for the store to hold documents and says how many.
//
// A crawl runs behind the call that started it on purpose, so every assertion
// about what is in the index has to wait for one. The deadline is generous
// because this is a real walk of a real directory on whatever machine the tests
// are running on.
func indexed(t *testing.T, st *memstore.Store, want int) int {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		seen := held(t, st)
		if seen >= want || !time.Now().Before(deadline) {
			return seen
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// held is how many documents the store has right now.
func held(t *testing.T, st *memstore.Store) int {
	t.Helper()
	stats, err := st.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return stats.Documents
}

// screen is one row of the administration screen, and fails the test if the
// connector is not on it.
func screen(t *testing.T, ops *operations, source string) api.Connector {
	t.Helper()
	for _, c := range ops.State().Connectors {
		if c.Source == source {
			return c
		}
	}
	t.Fatalf("%q is not on the administration screen", source)
	return api.Connector{}
}

// The whole point, in one test: a directory named through the interface is
// crawled, and it is still configured after the process that was told about it
// has gone.
func TestAConnectorAddedFromTheInterfaceIsCrawledAndRemembered(t *testing.T) {
	sup, st, ops := newTestSupervisor(t)

	if err := sup.Add(t.Context(), corpusFeedFor(t, "handbook", corpusTree(t), true)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if n := indexed(t, st, 1); n == 0 {
		t.Fatal("a connector was added and nothing was indexed")
	}

	// On the screen, and marked as something the screen may change.
	c := screen(t, ops, "handbook")
	if !c.Managed || !c.Enabled {
		t.Errorf("the connector reads as %+v, want it managed and enabled", c)
	}

	// And written down, which is the part a restart depends on.
	saved, err := st.Feeds(t.Context(), "acme")
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(saved) != 1 || saved[0].Source != "handbook" || saved[0].By != "u_ops" {
		t.Fatalf("the store holds %+v", saved)
	}
}

// Switching a source off keeps everything about it except the crawler, which is
// the difference between silencing a connector that is producing errors and
// having to type it all in again afterwards.
func TestStoppingAConnectorKeepsItsSettings(t *testing.T) {
	sup, st, ops := newTestSupervisor(t)

	if err := sup.Add(t.Context(), corpusFeedFor(t, "handbook", corpusTree(t), true)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	indexed(t, st, 1)

	if err := sup.Stop(t.Context(), "acme", "handbook"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if c := screen(t, ops, "handbook"); c.Enabled || c.Syncing {
		t.Errorf("a stopped connector reads as %+v", c)
	}
	saved, err := st.Feeds(t.Context(), "acme")
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(saved) != 1 || saved[0].Enabled {
		t.Fatalf("the store holds %+v, want the settings kept and switched off", saved)
	}

	// And on again, which is the same connector rather than a second one.
	if err := sup.Start(t.Context(), "acme", "handbook"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if c := screen(t, ops, "handbook"); !c.Enabled {
		t.Errorf("a started connector reads as %+v", c)
	}
	if got := len(ops.State().Connectors); got != 1 {
		t.Errorf("stopping and starting left %d connectors, want one", got)
	}
}

// The one that would be expensive to get wrong. Removing a connector forgets
// how a corpus was read and leaves the corpus, because the alternative makes an
// operator's undo cost a full crawl.
func TestRemovingAConnectorLeavesTheDocumentsItIndexed(t *testing.T) {
	sup, st, ops := newTestSupervisor(t)

	if err := sup.Add(t.Context(), corpusFeedFor(t, "handbook", corpusTree(t), true)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	before := indexed(t, st, 1)
	if before == 0 {
		t.Fatal("nothing was indexed to begin with")
	}

	if err := sup.Remove(t.Context(), "acme", "handbook"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if after := held(t, st); after != before {
		t.Errorf("removing the connector took the corpus with it: %d documents left of %d", after, before)
	}
	saved, err := st.Feeds(t.Context(), "acme")
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(saved) != 0 {
		t.Errorf("the configuration is still there: %+v", saved)
	}
	if got := ops.State().Connectors; len(got) != 0 {
		t.Errorf("the removed connector is still on the screen: %+v", got)
	}
}

// A connector that came off the command line is on the screen because it is
// running, and cannot be changed from there because the next restart would read
// the command line again and undo whatever was typed.
func TestAConnectorFromTheCommandLineCannotBeChangedFromTheInterface(t *testing.T) {
	sup, _, _ := newTestSupervisor(t)
	sup.fix("files")
	dir := corpusTree(t)

	for _, c := range []struct {
		name string
		call func() error
	}{
		{"add", func() error { return sup.Add(t.Context(), corpusFeedFor(t, "files", dir, true)) }},
		{"remove", func() error { return sup.Remove(t.Context(), "acme", "files") }},
		{"start", func() error { return sup.Start(t.Context(), "acme", "files") }},
		{"stop", func() error { return sup.Stop(t.Context(), "acme", "files") }},
		{"sync", func() error { return sup.Sync(t.Context(), "acme", "files") }},
	} {
		if err := c.call(); !errors.Is(err, api.ErrUnmanaged) {
			t.Errorf("%s returned %v, want it to say the connector is not managed here", c.name, err)
		}
	}
}

// Naming another tenant's connector is answered the same way as naming one that
// was never configured, because from the caller's side those are the same thing
// and the difference is what would tell them another deployment has it.
func TestAConnectorOfAnotherTenantIsNotFound(t *testing.T) {
	sup, _, _ := newTestSupervisor(t)
	if err := sup.Add(t.Context(), corpusFeedFor(t, "handbook", corpusTree(t), false)); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if err := sup.Start(t.Context(), "other", "handbook"); !errors.Is(err, genba.ErrNotFound) {
		t.Errorf("starting another tenant's connector returned %v", err)
	}
	if err := sup.Stop(t.Context(), "acme", "never-configured"); !errors.Is(err, genba.ErrNotFound) {
		t.Errorf("stopping a connector nobody configured returned %v", err)
	}
}

// Settings that cannot be run are refused before they are written down, and the
// message says which field is wrong. A connector that is accepted and then
// fails in a log line a minute later is the version of this nobody can use.
func TestSettingsThatCannotBeRunAreRefused(t *testing.T) {
	sup, st, _ := newTestSupervisor(t)
	gone := filepath.Join(t.TempDir(), "not-mounted")

	tests := []struct {
		name string
		feed store.Feed
		want string
	}{
		{
			"a kind nobody implements",
			store.Feed{Tenant: "acme", Source: "x", Kind: "confluence"},
			"confluence",
		},
		{
			"a connector with no kind at all",
			store.Feed{Tenant: "acme", Source: "x"},
			"kind",
		},
		{
			"a directory connector with no directory",
			store.Feed{Tenant: "acme", Source: "x", Kind: kindCorpus, Config: json.RawMessage(`{}`)},
			"dir",
		},
		{
			"a directory that is not there",
			store.Feed{Tenant: "acme", Source: "x", Kind: kindCorpus, Config: settings(t, corpusConfig{Dir: gone})},
			"not-mounted",
		},
		{
			"an interval that is not a duration",
			store.Feed{Tenant: "acme", Source: "x", Kind: kindCorpus, Config: settings(t, corpusConfig{Dir: gone, Refresh: "often"})},
			"duration",
		},
		{
			"settings that are not readable at all",
			store.Feed{Tenant: "acme", Source: "x", Kind: kindCorpus, Config: json.RawMessage(`[1,2,3]`)},
			"not readable",
		},
		{
			"a bucket with no bucket",
			store.Feed{Tenant: "acme", Source: "x", Kind: kindBucket, Config: json.RawMessage(`{}`)},
			"bucket",
		},
		{
			"a bucket with nowhere to send the request",
			store.Feed{Tenant: "acme", Source: "x", Kind: kindBucket, Config: json.RawMessage(`{"bucket":"docs"}`)},
			"endpoint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sup.Add(t.Context(), tt.feed)
			if !errors.Is(err, api.ErrBadConnector) {
				t.Fatalf("Add returned %v, want it refused as settings that cannot be run", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("the error is %q, want it to mention %q", err, tt.want)
			}
			saved, ferr := st.Feeds(t.Context(), "acme")
			if ferr != nil {
				t.Fatalf("Feeds: %v", ferr)
			}
			if len(saved) != 0 {
				t.Errorf("settings that were refused were written down anyway: %+v", saved)
			}
		})
	}
}

// A connector configured before this process came up starts with it, which is
// the whole reason the configuration lives in the store rather than in memory.
func TestConnectorsConfiguredBeforeTheRestartComeBack(t *testing.T) {
	st := newTestStore(t)
	root := corpusTree(t)

	// The first process configures one and switches another off.
	first := newSupervisor(st, st, "acme", credentials{}, newIndexing(t.Context(), st), newOperations(), quietLog())
	if err := first.Add(t.Context(), corpusFeedFor(t, "handbook", root, true)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := first.Add(t.Context(), corpusFeedFor(t, "archive", root, false)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	first.stop()

	// The second reads them back out of the store.
	ops := newOperations()
	second := newSupervisor(st, st, "acme", credentials{}, newIndexing(t.Context(), st), ops, quietLog())
	t.Cleanup(second.stop)
	if err := second.restore(t.Context()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if c := screen(t, ops, "handbook"); !c.Enabled {
		t.Errorf("the running connector came back as %+v", c)
	}
	// The one that was switched off is on the screen with an off switch rather
	// than gone from it, because a connector nobody can see is a connector
	// nobody can switch back on.
	if c := screen(t, ops, "archive"); c.Enabled {
		t.Errorf("a connector that was switched off came back running: %+v", c)
	}
}

// A stored connector that has since been given the same name on the command
// line does not start. Two crawlers writing the same document ids from two
// different directories leave an index that is whichever of them ran last.
func TestACommandLineConnectorWinsAClashOfNames(t *testing.T) {
	st := newTestStore(t)

	first := newSupervisor(st, st, "acme", credentials{}, newIndexing(t.Context(), st), newOperations(), quietLog())
	if err := first.Add(t.Context(), corpusFeedFor(t, "files", corpusTree(t), true)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	first.stop()

	ops := newOperations()
	second := newSupervisor(st, st, "acme", credentials{}, newIndexing(t.Context(), st), ops, quietLog())
	t.Cleanup(second.stop)
	second.fix("files")
	if err := second.restore(t.Context()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := ops.State().Connectors; len(got) != 0 {
		t.Errorf("the stored connector started anyway: %+v", got)
	}
}

// Asking a connector that is switched off for a sync says so, rather than
// answering as though a crawl is on its way.
func TestSyncingAConnectorThatIsSwitchedOffSaysSo(t *testing.T) {
	sup, _, _ := newTestSupervisor(t)
	if err := sup.Add(t.Context(), corpusFeedFor(t, "handbook", corpusTree(t), false)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := sup.Sync(t.Context(), "acme", "handbook"); !errors.Is(err, api.ErrBadConnector) {
		t.Fatalf("Sync returned %v, want it to say there is nothing to sync", err)
	}
}

// A connector's own context is not the context of the request that started it.
// The request is over in a millisecond and the crawl is not, so a crawler that
// ended with its request would index nothing and report success.
func TestACrawlOutlivesTheRequestThatStartedIt(t *testing.T) {
	sup, st, _ := newTestSupervisor(t)

	request, answered := context.WithCancel(t.Context())
	if err := sup.Add(request, corpusFeedFor(t, "handbook", corpusTree(t), true)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	answered()

	if n := indexed(t, st, 1); n == 0 {
		t.Fatal("the crawl was cancelled with the request that asked for it")
	}
}

// The end to end version: a running server, a connector added over HTTP by
// somebody holding the administrator role, and a query that finds what it
// crawled.
func TestAConnectorAddedOverTheAPIIsSearchable(t *testing.T) {
	addr := freeAddr(t)
	root := corpusTree(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, []string{
			"-addr", addr,
			"-tenant", "acme",
			"-store", "sqlite",
			"-dsn", filepath.Join(t.TempDir(), "genba.db"),
			"-admins", "u_ops",
			"-log-level", "error",
		}, env(nil), &out, &errOut)
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the server returned %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the server did not shut down")
		}
	}()
	waitForIndex(t, "http://"+addr)

	body := `{"source":"handbook","kind":"corpus","enabled":true,"config":{"dir":` +
		string(settings(t, root)) + `,"acl":"tenant"}}`
	if code := operate(t, http.MethodPost, "http://"+addr+"/api/v1/admin/connectors", body); code != http.StatusOK {
		t.Fatalf("adding a connector returned %d", code)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if searchAs(t, addr, "alice", "deploying").Total > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a connector was added over the API and a query about it found nothing")
}

// operate sends one administration request and returns the status.
func operate(t *testing.T, method, url, body string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Genba-Subject", "u_ops")
	req.Header.Set("X-Genba-Tenant", "acme")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Logf("%s %s: %s", method, url, out)
	}
	return resp.StatusCode
}
