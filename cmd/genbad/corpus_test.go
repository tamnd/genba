package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// corpusTree writes a small tree with an OWNERS file in it, which is what the
// two access control modes differ over.
func corpusTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"OWNERS":            "approvers:\n  - alice\n",
		"README.md":         "# The handbook\n\nHow we work.\n",
		"guides/deploy.md":  "# Deploying\n\nPush the button.\n",
		"loose/stray.md":    "# stray\n",
		"guides/OWNERS":     "approvers:\n  - bob\n",
		"guides/legacy.md":  "# Legacy\n\nOld notes.\n",
		"assets/logo.bin":   "\xff\xfe binary",
		"node_modules/x.js": "should not be read",
	}
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCorpusFlagsAreChecked(t *testing.T) {
	tests := []struct {
		name string
		opts corpusOptions
		want string
	}{
		{"nothing set is fine", corpusOptions{}, ""},
		{"a directory with no name", corpusOptions{Dir: "/tmp"}, "name is empty"},
		{"an unknown acl", corpusOptions{Dir: "/tmp", Name: "files", ACL: "everyone"}, "everyone"},
		{"a negative refresh", corpusOptions{Dir: "/tmp", Name: "files", ACL: aclTenant, Refresh: -time.Second}, "negative"},
		{"a usable set", corpusOptions{Dir: "/tmp", Name: "files", ACL: aclOwners}, ""},
		{"the os policy with nobody to name accounts", corpusOptions{Dir: "/tmp", Name: "files", ACL: aclOS}, "identity source"},
		{"the os policy told where the names come from", corpusOptions{Dir: "/tmp", Name: "files", ACL: aclOS, Identity: "unix"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("rejected a usable set: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted %+v", tt.opts)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error is %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A corpus has to be filed under a tenant, and there is nothing sensible to
// guess if one was not given.
func TestACorpusWithoutATenantIsRefused(t *testing.T) {
	args := []string{"-corpus", corpusTree(t), "-log-level", "error"}
	var out, errOut bytes.Buffer
	err := run(t.Context(), args, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("a corpus was ingested with no tenant to file it under")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("error is %q, want it to say what is missing", err)
	}
}

// The point of the whole thing: start the server on a directory and get real
// results back out of the API.
func TestAStartedServerAnswersQueriesAboutTheCorpus(t *testing.T) {
	root := corpusTree(t)
	addr := freeAddr(t)

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
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the server did not shut down")
		}
	}()

	waitForHealth(t, "http://"+addr+"/healthz")

	res := searchAs(t, addr, "alice", "deploying")
	if res.Total == 0 {
		t.Fatal("the corpus was ingested but a query about it found nothing")
	}
	var found bool
	for _, h := range res.Hits {
		if strings.HasSuffix(h.ID, "guides/deploy.md") {
			found = true
			if h.Title != "Deploying" {
				t.Errorf("title is %q, want the heading from the file", h.Title)
			}
		}
	}
	if !found {
		t.Errorf("results %v do not include the file the query names", res.Hits)
	}

	// The directories the walk skips stay out of the corpus, so a dependency
	// tree does not drown the content somebody actually wrote.
	if got := searchAs(t, addr, "alice", "should not be read"); got.Total != 0 {
		t.Errorf("a skipped directory was indexed: %v", got.Hits)
	}
}

// With the owners policy the answer to a query depends on who is asking, which
// is the difference between a search engine and a leak.
func TestOwnersDecideWhatAQueryReturns(t *testing.T) {
	root := corpusTree(t)
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, []string{
			"-addr", addr,
			"-tenant", "acme",
			"-corpus", root,
			"-corpus-name", "handbook",
			"-corpus-acl", aclOwners,
			"-log-level", "error",
		}, env(nil), &out, &errOut)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the server did not shut down")
		}
	}()

	waitForHealth(t, "http://"+addr+"/healthz")

	// bob approves the guides directory and alice approves the root, which the
	// guides OWNERS file replaced.
	if got := searchAs(t, addr, "bob", "legacy"); got.Total == 0 {
		t.Error("an approver of the directory cannot see a file in it")
	}
	if got := searchAs(t, addr, "alice", "legacy"); got.Total != 0 {
		t.Errorf("a file in a subtree that replaced alice came back for her: %v", got.Hits)
	}
	// Nobody is named in an OWNERS file above the root, so the root file governs
	// the top level and alice can read it.
	if got := searchAs(t, addr, "alice", "handbook"); got.Total == 0 {
		t.Error("an approver of the root cannot see a file at the root")
	}
	// A stranger sees none of it.
	if got := searchAs(t, addr, "mallory", "legacy"); got.Total != 0 {
		t.Errorf("somebody named in no OWNERS file got results: %v", got.Hits)
	}
}

// A file somebody deleted has to stop coming back, and a walk of the tree can
// never see one: there is nothing left to walk past. This is the sweep doing
// the job the incremental path cannot do at all.
func TestADeletedFileStopsComingBack(t *testing.T) {
	root := corpusTree(t)
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, []string{
			"-addr", addr,
			"-tenant", "acme",
			"-corpus", root,
			"-corpus-name", "handbook",
			"-corpus-refresh", "100ms",
			"-log-level", "error",
		}, env(nil), &out, &errOut)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the server did not shut down")
		}
	}()

	waitForHealth(t, "http://"+addr+"/healthz")

	if got := searchAs(t, addr, "alice", "deploying"); got.Total == 0 {
		t.Fatal("the file was not indexed in the first place")
	}
	if err := os.Remove(filepath.Join(root, "guides", "deploy.md")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if got := searchAs(t, addr, "alice", "deploying"); got.Total == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a file deleted from the tree is still in the results")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// And the rest of the corpus is still there, which is the part worth
	// checking twice. A sweep that deletes too much looks the same in this test
	// as one that works, right up until nothing can be found.
	if got := searchAs(t, addr, "alice", "handbook"); got.Total == 0 {
		t.Error("the sweep removed documents the tree still holds")
	}
}

type hit struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type searchResult struct {
	Total int   `json:"total"`
	Hits  []hit `json:"hits"`
}

// searchAs runs a query as one person and returns what came back.
func searchAs(t *testing.T, addr, subject, query string) searchResult {
	t.Helper()
	url := "http://" + addr + "/api/v1/search?q=" + strings.ReplaceAll(query, " ", "+")
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Genba-Subject", subject)
	req.Header.Set("X-Genba-Tenant", "acme")
	req.Header.Set("X-Genba-Identities", "github:"+subject)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search returned %s", resp.Status)
	}
	var out searchResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	return out
}
