package recorded_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tamnd/genba/connector/recorded"
)

// service is a stand in for the product a connector talks to. Everything in
// this file records against it and then replays without it, which is the whole
// claim the package makes.
func service(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// answer is a response that has already been read and closed, which is what
// every helper here hands back so that no test has to remember to.
type answer struct {
	status int
	header http.Header
	body   string
}

// do makes a request and reads all of the answer to it. The error is returned
// rather than fatal, because a request the recording has nothing for is the
// point of several of these tests.
func do(t *testing.T, c *http.Client, req *http.Request) (answer, error) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{status: resp.StatusCode, header: resp.Header, body: string(body)}, nil
}

// get asks with a credential on it, the way a connector does.
func get(t *testing.T, c *http.Client, rawurl string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawurl, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer xoxb-a-real-token")
	got, err := do(t, c, req)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// post asks with a form, the way several of these products want to be asked.
func post(t *testing.T, c *http.Client, rawurl string, form url.Values) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, rawurl, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	got, err := do(t, c, req)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWhatWasRecordedIsWhatComesBack(t *testing.T) {
	srv := service(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Rate-Limit-Remaining", "48")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, `{"ok":true,"channels":[{"id":"C1","name":"maintenance"}]}`)
	})

	rec := recorded.Record(http.DefaultTransport)
	live := get(t, rec.Client(), srv.URL+"/api/conversations.list?limit=200")

	dir := t.TempDir()
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}

	rt, err := recorded.Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()

	replayed := get(t, rt.Client(), srv.URL+"/api/conversations.list?limit=200")
	// The same document rather than the same bytes. A JSON body is nested into
	// the file as JSON so that a diff can be read, which reindents it, and what
	// is being recorded is what the service said rather than its whitespace.
	sameJSON(t, replayed.body, live.body)
	if replayed.status != live.status {
		t.Errorf("status is %d, want %d", replayed.status, live.status)
	}
	// The rate limit headers are the ones a connector's backoff reads, so a
	// recording that dropped them could not be used to test the thing most
	// likely to be wrong.
	if got := replayed.header.Get("X-Rate-Limit-Remaining"); got != "48" {
		t.Errorf("the rate limit header came back as %q", got)
	}
	if got := replayed.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content type is %q", got)
	}
}

// A recording is committed. A token committed once is a token leaked
// permanently, whatever the next commit does.
func TestNoCredentialReachesTheFile(t *testing.T) {
	srv := service(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"webhook":"https://hooks.example.com/T1/B2/xoxb-secret-hook"}`)
	})

	rec := recorded.Record(http.DefaultTransport, recorded.WithScrubber(func(b []byte) []byte {
		return []byte(strings.ReplaceAll(string(b), "xoxb-secret-hook", recorded.Redacted))
	}))
	get(t, rec.Client(), srv.URL+"/api/auth.test?token=xoxb-a-real-token&limit=1")

	dir := t.TempDir()
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{"xoxb-a-real-token", "xoxb-secret-hook"} {
		if found := grep(t, dir, secret); found != "" {
			t.Errorf("%s carries %q:\n%s", found, secret, read(t, filepath.Join(dir, found)))
		}
	}
	// What is left has to still say a token was there, or a reader cannot tell
	// a header that was removed from one the service never sent.
	if !strings.Contains(read(t, filepath.Join(dir, only(t, dir))), recorded.Redacted) {
		t.Error("the recording does not say anything was removed")
	}
}

// A recording made with a token has to answer a test that has none, or the
// fixture set is only usable by whoever recorded it.
func TestARecordingMadeWithATokenReplaysWithoutOne(t *testing.T) {
	srv := service(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	rec := recorded.Record(http.DefaultTransport)
	get(t, rec.Client(), srv.URL+"/api/auth.test?token=xoxb-a-real-token")

	dir := t.TempDir()
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	rt, err := recorded.Replay(dir)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/auth.test", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := do(t, rt.Client(), req); err != nil {
		t.Fatalf("a request with no token found nothing: %v", err)
	}
}

// A recording is a description of an API rather than of the workspace it was
// taken from, so a test does not have to point its client at whichever
// hostname somebody happened to record against.
func TestTheHostIsNotPartOfTheQuestion(t *testing.T) {
	rt := recorded.From([]recorded.Exchange{{
		Request:  recorded.Request{Method: "GET", URL: "https://acme.example.com/api/conversations.list?limit=200"},
		Response: recorded.Response{Status: 200, Body: recorded.Payload{Text: "ok"}},
	}})

	if got := get(t, rt.Client(), "https://somewhere-else.invalid/api/conversations.list?limit=200"); got.body != "ok" {
		t.Errorf("body is %q", got.body)
	}
}

func TestTheOrderOfTheQueryDoesNotMatter(t *testing.T) {
	rt := recorded.From([]recorded.Exchange{{
		Request:  recorded.Request{Method: "GET", URL: "https://x/api/list?cursor=abc&limit=200&types=public"},
		Response: recorded.Response{Status: 200, Body: recorded.Payload{Text: "ok"}},
	}})

	if got := get(t, rt.Client(), "https://x/api/list?types=public&limit=200&cursor=abc"); got.body != "ok" {
		t.Errorf("body is %q", got.body)
	}
}

// Several of these products ask with a form post rather than a query string,
// and the order a client writes its fields in is not part of what it asked.
func TestAFormPostIsComparedFieldByField(t *testing.T) {
	srv := service(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"messages":[]}`)
	})

	rec := recorded.Record(http.DefaultTransport)
	post(t, rec.Client(), srv.URL+"/api/conversations.replies", url.Values{
		"channel": {"C1"}, "ts": {"1714816800.000100"}, "limit": {"200"},
	})

	dir := t.TempDir()
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	rt, err := recorded.Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()

	got := post(t, rt.Client(), srv.URL+"/api/conversations.replies", url.Values{
		"ts": {"1714816800.000100"}, "limit": {"200"}, "channel": {"C1"},
	})
	sameJSON(t, got.body, `{"ok":true,"messages":[]}`)
}

func TestADifferentFormPostIsADifferentQuestion(t *testing.T) {
	rt := recorded.From([]recorded.Exchange{{
		Request: recorded.Request{
			Method: "POST",
			URL:    "https://x/api/conversations.replies",
			Body:   recorded.Payload{Text: "channel=C1&ts=1"},
		},
		Response: recorded.Response{Status: 200, Body: recorded.Payload{Text: "ok"}},
	}})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://x/api/conversations.replies", strings.NewReader("channel=C2&ts=1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := do(t, rt.Client(), req); err == nil {
		t.Fatal("a request for another channel was answered from the recording for C1")
	}
}

// A crawl that pages asks the same question twice with different answers, and a
// sync run twice asks it again and should get the last one rather than an
// error.
func TestRepeatedRequestsAreAnsweredInOrderAndThenRepeat(t *testing.T) {
	page := func(body string) recorded.Exchange {
		return recorded.Exchange{
			Request:  recorded.Request{Method: "GET", URL: "https://x/api/list"},
			Response: recorded.Response{Status: 200, Body: recorded.Payload{Text: body}},
		}
	}
	rt := recorded.From([]recorded.Exchange{page("first"), page("second")})

	want := []string{"first", "second", "second", "second"}
	for i, w := range want {
		if got := get(t, rt.Client(), "https://x/api/list"); got.body != w {
			t.Errorf("request %d got %q, want %q", i+1, got.body, w)
		}
	}
}

// A request nothing was recorded for is an error naming what was asked and what
// the recording holds, rather than an empty response the connector then fails
// to parse fifty lines away from the cause.
func TestAnUnrecordedRequestSaysWhatItAskedAndWhatIsThere(t *testing.T) {
	rt := recorded.From([]recorded.Exchange{{
		Request:  recorded.Request{Method: "GET", URL: "https://x/api/conversations.list"},
		Response: recorded.Response{Status: 200, Body: recorded.Payload{Text: "ok"}},
	}})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://x/api/users.list", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	_, err = do(t, rt.Client(), req)
	if err == nil {
		t.Fatal("a request nothing was recorded for was answered")
	}
	if !errors.Is(err, recorded.ErrNoRecording) {
		t.Errorf("error is %v, want one that says nothing was recorded", err)
	}
	if !strings.Contains(err.Error(), "/api/users.list") {
		t.Errorf("the error does not say what was asked: %v", err)
	}
	if !strings.Contains(err.Error(), "/api/conversations.list") {
		t.Errorf("the error does not say what the recording holds: %v", err)
	}
}

// A fixture set drifts the same way a comment does. A connector that stopped
// calling an endpoint leaves the recording of it behind, and the next person to
// read the directory takes it for a description of what the connector does.
func TestUnusedReportsWhatNothingAskedFor(t *testing.T) {
	rt := recorded.From([]recorded.Exchange{
		{
			Request:  recorded.Request{Method: "GET", URL: "https://x/api/conversations.list"},
			Response: recorded.Response{Status: 200, Body: recorded.Payload{Text: "ok"}},
		},
		{
			Request:  recorded.Request{Method: "GET", URL: "https://x/api/users.list"},
			Response: recorded.Response{Status: 200, Body: recorded.Payload{Text: "ok"}},
		},
	})

	if got := rt.Unused(); len(got) != 2 {
		t.Fatalf("before anything was asked, Unused is %v", got)
	}
	get(t, rt.Client(), "https://x/api/conversations.list")

	got := rt.Unused()
	if len(got) != 1 || !strings.Contains(got[0], "users.list") {
		t.Errorf("Unused is %v, want the one nothing asked for", got)
	}
}

// A recording is reviewed, and a review of a wall of base64 is not a review.
func TestAJsonBodyIsWrittenAsJsonSoThatADiffCanBeRead(t *testing.T) {
	srv := service(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"channels":[{"id":"C1","name":"maintenance","is_private":false}]}`)
	})

	rec := recorded.Record(http.DefaultTransport)
	get(t, rec.Client(), srv.URL+"/api/conversations.list")

	dir := t.TempDir()
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}

	file := read(t, filepath.Join(dir, only(t, dir)))
	if !strings.Contains(file, `"name": "maintenance"`) {
		t.Errorf("the response was not written out as readable json:\n%s", file)
	}
	if strings.Contains(file, "base64") || strings.Contains(file, `\"`) {
		t.Errorf("the response was escaped into a string:\n%s", file)
	}
}

func TestABodyThatIsNotTextRoundTrips(t *testing.T) {
	want := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0xff, 0xfe}
	srv := service(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(want)
	})

	rec := recorded.Record(http.DefaultTransport)
	get(t, rec.Client(), srv.URL+"/files/logo.png")

	dir := t.TempDir()
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}
	rt, err := recorded.Replay(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()

	if got := get(t, rt.Client(), srv.URL+"/files/logo.png"); got.body != string(want) {
		t.Errorf("the bytes came back as %q, want %q", got.body, want)
	}
}

// Refreshing a fixture set against a service that no longer has an endpoint has
// to leave the directory describing the crawl as it is now, and has to leave
// alone whatever else is in there.
func TestSavingAgainReplacesTheOldRecordingAndNothingElse(t *testing.T) {
	srv := service(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	dir := t.TempDir()

	first := recorded.Record(http.DefaultTransport)
	get(t, first.Client(), srv.URL+"/api/one")
	get(t, first.Client(), srv.URL+"/api/two")
	get(t, first.Client(), srv.URL+"/api/three")
	if err := first.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("how this was taken"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := recorded.Record(http.DefaultTransport)
	get(t, second.Client(), srv.URL+"/api/one")
	if err := second.Save(dir); err != nil {
		t.Fatal(err)
	}

	names := files(t, dir)
	if len(names) != 2 {
		t.Fatalf("the directory holds %v", names)
	}
	if !contains(names, "README.md") {
		t.Errorf("the note beside the recordings was removed: %v", names)
	}
	for _, name := range names {
		if strings.Contains(name, "two") || strings.Contains(name, "three") {
			t.Errorf("%s survived a refresh that no longer makes that request", name)
		}
	}
}

// The number in front is the order the requests were made in, because a crawl
// is a sequence and a listing sorted alphabetically would put the second page
// of results before the first.
func TestTheFilesAreNamedForWhatWasAskedInTheOrderItWasAsked(t *testing.T) {
	srv := service(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	rec := recorded.Record(http.DefaultTransport)
	get(t, rec.Client(), srv.URL+"/api/conversations.list?limit=200")
	get(t, rec.Client(), srv.URL+"/api/conversations.history?channel=C1")

	dir := t.TempDir()
	if err := rec.Save(dir); err != nil {
		t.Fatal(err)
	}

	names := files(t, dir)
	if len(names) != 2 {
		t.Fatalf("the directory holds %v", names)
	}
	if !strings.HasPrefix(names[0], "0001-") || !strings.Contains(names[0], "conversations.list") {
		t.Errorf("the first file is %q", names[0])
	}
	if !strings.HasPrefix(names[1], "0002-") || !strings.Contains(names[1], "conversations.history") {
		t.Errorf("the second file is %q", names[1])
	}
	if strings.ContainsAny(strings.Join(names, ""), `/\:?*"<>|`) {
		t.Errorf("a name holds something a filesystem somewhere will refuse: %v", names)
	}
}

func TestReplayingSomethingThatIsNotThereSaysSo(t *testing.T) {
	if _, err := recorded.Replay(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("replaying a directory that is not there succeeded")
	}
	if _, err := recorded.Replay(t.TempDir()); err == nil {
		t.Error("replaying an empty directory succeeded")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0001-broken.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := recorded.Replay(dir)
	if err == nil {
		t.Fatal("replaying a recording that does not parse succeeded")
	}
	if !strings.Contains(err.Error(), "0001-broken.json") {
		t.Errorf("the error does not say which file: %v", err)
	}
}

// A connector that fetches in parallel is a connector whose tests have to be
// able to. Run with -race this is the whole of the claim.
func TestATransportIsSafeToShareBetweenGoroutines(t *testing.T) {
	rt := recorded.From([]recorded.Exchange{{
		Request:  recorded.Request{Method: "GET", URL: "https://x/api/list"},
		Response: recorded.Response{Status: 200, Body: recorded.Payload{Text: "ok"}},
	}})

	c := rt.Client()
	done := make(chan string, 8)
	for range cap(done) {
		go func() {
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://x/api/list", http.NoBody)
			if err != nil {
				done <- err.Error()
				return
			}
			resp, err := c.Do(req)
			if err != nil {
				done <- err.Error()
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			done <- string(body)
		}()
	}
	for range cap(done) {
		if got := <-done; got != "ok" {
			t.Errorf("a concurrent request got %q", got)
		}
	}
}

// sameJSON reports the two bodies as the same document, whatever the
// whitespace between the fields is.
func sameJSON(t *testing.T, got, want string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		t.Fatalf("the body that came back is not json: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("the body wanted is not json: %v\n%s", err, want)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the body is\n%s\nand should have been\n%s", got, want)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func only(t *testing.T, dir string) string {
	t.Helper()
	names := files(t, dir)
	if len(names) != 1 {
		t.Fatalf("the directory holds %v, want one recording", names)
	}
	return names[0]
}

// grep returns the name of the first file in dir holding a string, or empty.
func grep(t *testing.T, dir, want string) string {
	t.Helper()
	for _, name := range files(t, dir) {
		if strings.Contains(read(t, filepath.Join(dir, name)), want) {
			return name
		}
	}
	return ""
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
