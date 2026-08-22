package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBucketFlagsAreChecked(t *testing.T) {
	usable := bucketOptions{Bucket: "corpus", Endpoint: "https://s3.eu-west-1.amazonaws.com", Name: "objects", ACL: aclTenant}
	with := func(f func(*bucketOptions)) bucketOptions {
		o := usable
		f(&o)
		return o
	}

	tests := []struct {
		name string
		opts bucketOptions
		want string
	}{
		{"nothing set is fine", bucketOptions{}, ""},
		{"a usable set", usable, ""},
		{"a bucket with nowhere to read it from", with(func(o *bucketOptions) { o.Endpoint = "" }), "endpoint"},
		{"a bucket with no source name", with(func(o *bucketOptions) { o.Name = "" }), "name is empty"},
		{"an unknown acl", with(func(o *bucketOptions) { o.ACL = "everyone" }), "everyone"},
		{"the bucket acl with nobody to name accounts", with(func(o *bucketOptions) { o.ACL = aclBucket }), "identity source"},
		{"the bucket acl told where the names come from", with(func(o *bucketOptions) { o.ACL, o.Identity = aclBucket, "google" }), ""},
		{"the object acl with nobody to name accounts", with(func(o *bucketOptions) { o.ACL = aclObject }), "identity source"},
		{"a key with no secret", with(func(o *bucketOptions) { o.Access = "AKIA" }), "together"},
		{"a secret with no key", with(func(o *bucketOptions) { o.Secret = "shhh" }), "together"},
		{"both of them", with(func(o *bucketOptions) { o.Access, o.Secret = "AKIA", "shhh" }), ""},
		{"a negative refresh", with(func(o *bucketOptions) { o.Refresh = -time.Second }), "negative"},
		{"a negative reconcile interval", with(func(o *bucketOptions) { o.Reconcile = -time.Second }), "negative"},
		{"a negative rate", with(func(o *bucketOptions) { o.Rate = -1 }), "negative"},
		{"a negative burst", with(func(o *bucketOptions) { o.Burst = -1 }), "negative"},
		{"retrying turned off", with(func(o *bucketOptions) { o.Retries = -1 }), ""},
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

// Credentials come from the environment and there is no flag for them, because
// argv is readable by every process on the machine.
func TestBucketCredentialsComeFromTheEnvironment(t *testing.T) {
	var o bucketOptions
	o.credentials(env(map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAIOSFODNN7EXAMPLE",
		"AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"AWS_SESSION_TOKEN":     "temporary",
	}))
	if o.Access != "AKIAIOSFODNN7EXAMPLE" || o.Secret == "" || o.Session != "temporary" {
		t.Fatalf("credentials = %+v", o)
	}

	var out, errOut bytes.Buffer
	err := run(t.Context(), []string{"-bucket-secret-access-key", "shhh"}, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("a flag was accepted for the secret key")
	}
}

func TestABucketWithoutATenantIsRefused(t *testing.T) {
	args := []string{"-bucket", "corpus", "-bucket-endpoint", "https://example.invalid", "-log-level", "error"}
	var out, errOut bytes.Buffer
	err := run(t.Context(), args, env(nil), &out, &errOut)
	if err == nil {
		t.Fatal("a bucket was ingested with no tenant to file it under")
	}
	if !strings.Contains(err.Error(), "tenant") {
		t.Errorf("error is %q, want it to say what is missing", err)
	}
}

// The point of the whole thing: start the server on a bucket and get real
// results back out of the API.
func TestAStartedServerAnswersQueriesAboutTheBucket(t *testing.T) {
	store := newBucket(t)
	store.put("handbook/README.md", "# The handbook\n\nHow we work.\n")
	store.put("handbook/guides/deploy.md", "# Deploying\n\nPush the button.\n")
	store.put("elsewhere/private.md", "# Private\n\nNot in the prefix.\n")

	addr := freeAddr(t)
	stop := serve(t, addr,
		"-bucket", theBucket,
		"-bucket-endpoint", store.url,
		"-bucket-path-style",
		"-bucket-prefix", "handbook/",
		"-bucket-name", "docs",
		"-bucket-refresh", "100ms",
	)
	defer stop()

	waitForIndex(t, "http://"+addr)

	res := searchAs(t, addr, "alice", "deploying")
	if res.Total == 0 {
		t.Fatal("the bucket was ingested but a query about it found nothing")
	}
	var found bool
	for _, h := range res.Hits {
		if strings.HasSuffix(h.ID, "handbook/guides/deploy.md") {
			found = true
			if h.Title != "Deploying" {
				t.Errorf("title is %q, want the heading from the object", h.Title)
			}
		}
	}
	if !found {
		t.Errorf("results %v do not include the object the query names", res.Hits)
	}

	// The prefix narrows the listing rather than filtering it afterwards, so
	// what is outside it was never read.
	if got := searchAs(t, addr, "alice", "not in the prefix"); got.Total != 0 {
		t.Errorf("an object outside the prefix was indexed: %v", got.Hits)
	}

	// An object written after the server started is picked up by the next
	// listing, and one removed goes on the sweep that follows it.
	store.put("handbook/guides/rollback.md", "# Rolling back\n\nPress the other button.\n")
	waitForResults(t, addr, "rolling back", true)

	store.remove("handbook/guides/deploy.md")
	waitForResults(t, addr, "deploying", false)

	if got := searchAs(t, addr, "alice", "handbook"); got.Total == 0 {
		t.Error("the rest of the bucket went missing")
	}
}

// A server pointed at a directory and a bucket at once indexes both, and a
// query reaches across the two.
func TestAServerReadsADirectoryAndABucketTogether(t *testing.T) {
	store := newBucket(t)
	store.put("notes.md", "# Notes\n\nSomething only the bucket has.\n")

	addr := freeAddr(t)
	stop := serve(t, addr,
		"-corpus", corpusTree(t),
		"-corpus-name", "handbook",
		"-bucket", theBucket,
		"-bucket-endpoint", store.url,
		"-bucket-path-style",
		"-bucket-name", "docs",
	)
	defer stop()

	waitForIndex(t, "http://"+addr)

	if got := searchAs(t, addr, "alice", "deploying"); got.Total == 0 {
		t.Error("the directory was not indexed")
	}
	if got := searchAs(t, addr, "alice", "only the bucket has"); got.Total == 0 {
		t.Error("the bucket was not indexed")
	}
}

// A bucket that cannot be reached is a warning and a server that is still
// serving, because the other source is still good and an empty index that says
// nothing about why is worse than one that is missing a feed and says so.
func TestABucketThatRefusesDoesNotStopTheServer(t *testing.T) {
	refuser := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer refuser.Close()

	addr := freeAddr(t)
	stop := serve(t, addr,
		"-corpus", corpusTree(t),
		"-corpus-name", "handbook",
		"-bucket", theBucket,
		"-bucket-endpoint", refuser.URL,
		"-bucket-path-style",
	)
	defer stop()

	waitForIndex(t, "http://"+addr)

	if got := searchAs(t, addr, "alice", "deploying"); got.Total == 0 {
		t.Error("a bucket that refused took the directory down with it")
	}
}

// The crawl keeps itself under the rate it was given, which is the difference
// between a connector that is welcome to keep reading and one whose credentials
// get revoked while nobody is watching.
func TestTheBucketCrawlStaysUnderItsRate(t *testing.T) {
	store := newBucket(t)
	for _, key := range []string{"a.md", "b.md", "c.md", "d.md"} {
		store.put(key, "# "+key+"\n\nSomething worth reading.\n")
	}

	addr := freeAddr(t)
	began := time.Now()
	stop := serve(t, addr,
		"-bucket", theBucket,
		"-bucket-endpoint", store.url,
		"-bucket-path-style",
		"-bucket-rate", "20",
		"-bucket-burst", "1",
	)
	defer stop()

	waitForIndex(t, "http://"+addr)
	took := time.Since(began)

	hits := store.hits()
	if hits < 5 {
		t.Fatalf("the crawl made %d requests, want at least a listing and the four objects", hits)
	}
	// One request went out on the burst and every one after it waited fifty
	// milliseconds, so a crawl that finished sooner than that was not limited at
	// all. Against an in process service the same work takes single figures of
	// milliseconds unlimited, so there is no way for this to pass by accident.
	want := time.Duration(hits-1) * 50 * time.Millisecond
	if took < want {
		t.Errorf("the crawl took %v for %d requests, want at least %v", took, hits, want)
	}

	if got := searchAs(t, addr, "alice", "something worth reading"); got.Total < 4 {
		t.Errorf("total = %d, want the four objects to still be indexed", got.Total)
	}
}

// serve starts the server with the given flags and returns the function that
// stops it and waits for it to be gone.
func serve(t *testing.T, addr string, args ...string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		var out, errOut bytes.Buffer
		done <- run(ctx, append([]string{
			"-addr", addr,
			"-tenant", "acme",
			"-log-level", "error",
		}, args...), env(nil), &out, &errOut)
	}()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the server stopped with %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the server did not shut down")
		}
	}
}

// theBucket is the name the fake answers to.
const theBucket = "corpus"

// bucketFake is an S3 compatible service with one bucket in it.
//
// It is here rather than in a container because what these tests are about is
// the wiring: that the flags reach a client, that the client reaches a service,
// and that what comes back is searchable. What a real service would add is
// whether it accepts these signatures and returns this XML, and that is checked
// against the published examples in connector/objectsource.
type bucketFake struct {
	url string

	mu       sync.Mutex
	objects  map[string]string
	clock    time.Time
	requests int
}

func newBucket(t *testing.T) *bucketFake {
	t.Helper()
	f := &bucketFake{
		objects: make(map[string]string),
		clock:   time.Date(2026, time.March, 2, 12, 0, 0, 0, time.UTC),
	}
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

func (f *bucketFake) put(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = body
	// Every write moves the clock on, so that an object written after the last
	// sync is genuinely newer than the cursor rather than being in the same
	// second as it.
	f.clock = f.clock.Add(time.Minute)
}

func (f *bucketFake) remove(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	f.clock = f.clock.Add(time.Minute)
}

// hits is how many requests the fake has answered, which is what a rate is
// measured against.
func (f *bucketFake) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *bucketFake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests++
	w.Header().Set("Date", f.clock.UTC().Format(http.TimeFormat))
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/"+theBucket), "/")
	if unescaped, err := url.PathUnescape(key); err == nil {
		key = unescaped
	}

	switch {
	case r.URL.Query().Get("list-type") == "2":
		f.list(w, r.URL.Query().Get("prefix"))
	case key == "":
		http.Error(w, "no key", http.StatusNotFound)
	default:
		body, ok := f.objects[key]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>not here</Message></Error>`))
			return
		}
		w.Header().Set("Last-Modified", f.clock.UTC().Format(http.TimeFormat))
		w.Header().Set("ETag", `"`+key+`"`)
		_, _ = w.Write([]byte(body))
	}
}

// list answers a ListObjectsV2 in one page, which is all these corpora need.
func (f *bucketFake) list(w http.ResponseWriter, prefix string) {
	type contents struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
	}
	type result struct {
		XMLName     xml.Name   `xml:"ListBucketResult"`
		IsTruncated bool       `xml:"IsTruncated"`
		Contents    []contents `xml:"Contents"`
	}

	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	// A listing is ordered by key, and the connector's resumption depends on
	// that, so the fake had better be too.
	sort.Strings(keys)

	out := result{}
	for _, k := range keys {
		out.Contents = append(out.Contents, contents{
			Key:          k,
			LastModified: f.clock.UTC().Format(time.RFC3339),
			ETag:         `"` + k + `"`,
			Size:         int64(len(f.objects[k])),
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}
