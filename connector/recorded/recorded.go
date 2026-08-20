// Package recorded replays HTTP exchanges that were captured from a real
// service, so that a connector's tests do not need an account at one.
//
// A connector for somebody else's product is mostly a reading of that product's
// API, and the tests that matter are the ones that say what happens when it
// answers the way it really does. There are three ways to get that and two of
// them are bad. Hand written JSON says what somebody thought the API returns,
// which is how a connector ends up parsing a field that was renamed two years
// ago. A live account makes the test suite depend on a network, a token, a
// workspace somebody has to keep populated, and a rate limit, and the test then
// fails for four reasons that have nothing to do with the change under review.
//
// The third way is to talk to the real service once, write down exactly what it
// said, and run the tests against that. The recording is a file in the
// repository, so it is reviewed like code, it is diffed when it is refreshed,
// and the day the service changes a field the diff is the notice. Nothing in
// the test suite needs a token, and everything in it is checking against bytes
// a real service produced.
//
// # Recording
//
// Recording is a round tripper wrapped around a real one.
//
//	rec := recorded.Record(http.DefaultTransport)
//	src := chat.New(rec.Client(), token)
//	// ... drive the connector against the live service ...
//	if err := rec.Save("testdata/chat"); err != nil {
//		t.Fatal(err)
//	}
//
// Secrets never reach the file. The headers and query parameters that carry
// credentials are replaced before anything is written, and a [Scrubber] handles
// the ones that are in a body instead. This matters more than it sounds:
// a recording is committed, and a token committed once is a token leaked
// permanently, whatever the next commit does.
//
// # Replaying
//
// Replaying is the same round tripper with a directory behind it instead of a
// network.
//
//	rt, err := recorded.Replay("testdata/chat")
//
// A request nothing was recorded for is an error naming what was asked and what
// the recording holds, rather than an empty response the connector then fails
// to parse fifty lines away from the cause.
package recorded

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Redacted is what replaces a credential in a recording.
//
// It is a visible string rather than an empty one so that a reader of the file
// can tell a header that was removed from a header the service never sent, and
// so that a connector replaying against the recording still gets a header of
// the right shape.
const Redacted = "REDACTED"

// DefaultRedactedHeaders are the request and response headers that carry a
// credential often enough to be worth removing without being asked.
var DefaultRedactedHeaders = []string{
	"Authorization",
	"Cookie",
	"Proxy-Authorization",
	"Set-Cookie",
	"X-Api-Key",
	"X-Auth-Token",
}

// DefaultRedactedParams are the query parameters that carry a credential.
//
// They are removed from the file and from the key an exchange is matched on,
// which is the part that is easy to miss: a recording made with one token has
// to replay for a test that has no token at all.
var DefaultRedactedParams = []string{
	"access_token",
	"api_key",
	"key",
	"sig",
	"signature",
	"token",
	"x-amz-credential",
	"x-amz-signature",
}

// Scrubber removes whatever a body carries that must not be committed. It is
// handed the payload and returns what to write in its place.
type Scrubber func(body []byte) []byte

// Transport is an [http.RoundTripper] that either records what a real service
// said or replays what it said last time.
//
// It is safe for concurrent use, because a connector that fetches in parallel
// is a connector whose tests have to be able to.
type Transport struct {
	base    http.RoundTripper
	headers []string
	params  []string
	scrub   Scrubber

	mu        sync.Mutex
	recording bool
	exchanges []Exchange
	served    []int
	used      map[string]int
}

// Option configures a transport.
type Option func(*Transport)

// WithRedactedHeaders adds to the headers removed from a recording.
func WithRedactedHeaders(names ...string) Option {
	return func(t *Transport) { t.headers = append(t.headers, names...) }
}

// WithRedactedParams adds to the query parameters removed from a recording and
// from the key an exchange is matched on.
func WithRedactedParams(names ...string) Option {
	return func(t *Transport) { t.params = append(t.params, names...) }
}

// WithScrubber installs a function that removes secrets from a body.
//
// The default does nothing, because what is secret in a body is a fact about
// one product's API rather than about HTTP. A workspace that returns its own
// invite links or a signed download URL in a listing needs one of these.
func WithScrubber(s Scrubber) Option {
	return func(t *Transport) {
		if s != nil {
			t.scrub = s
		}
	}
}

// Record returns a transport that sends requests through base and remembers
// both sides of every one of them.
func Record(base http.RoundTripper, opts ...Option) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	t := newTransport(opts)
	t.base = base
	t.recording = true
	return t
}

// Replay returns a transport that answers out of the exchanges recorded in dir.
func Replay(dir string, opts ...Option) (*Transport, error) {
	exchanges, err := Load(dir)
	if err != nil {
		return nil, err
	}
	t := newTransport(opts)
	t.exchanges = exchanges
	return t, nil
}

// From returns a transport that answers out of exchanges held in memory, which
// is what a test that builds its own fixture rather than reading one wants.
func From(exchanges []Exchange, opts ...Option) *Transport {
	t := newTransport(opts)
	t.exchanges = slices.Clone(exchanges)
	return t
}

func newTransport(opts []Option) *Transport {
	t := &Transport{
		headers: slices.Clone(DefaultRedactedHeaders),
		params:  slices.Clone(DefaultRedactedParams),
		scrub:   func(b []byte) []byte { return b },
		used:    make(map[string]int),
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

var _ http.RoundTripper = (*Transport)(nil)

// Client returns an HTTP client using this transport.
func (t *Transport) Client() *http.Client { return &http.Client{Transport: t} }

// ErrNoRecording is returned for a request the recording has no answer for.
var ErrNoRecording = errors.New("recorded: nothing was recorded for this request")

// RoundTrip answers a request from the recording, or makes it and records the
// answer.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := take(req.Body)
	if err != nil {
		return nil, err
	}
	if t.recording {
		return t.capture(req, body)
	}
	return t.replay(req, body)
}

// replay finds the recorded answer to a request.
func (t *Transport) replay(req *http.Request, body []byte) (*http.Response, error) {
	k := t.key(req.Method, req.URL, body)

	t.mu.Lock()
	defer t.mu.Unlock()

	var found []int
	for i, e := range t.exchanges {
		u, err := url.Parse(e.Request.URL)
		if err != nil {
			continue
		}
		if t.key(e.Request.Method, u, e.Request.Body.Bytes()) == k {
			found = append(found, i)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%w: %s\nthe recording holds:\n%s", ErrNoRecording, k, t.summary())
	}

	// Several recordings of the same request are answered in the order they
	// were made, and the last one answers for ever after. A source asked the
	// same question twice usually gets the same answer, and a fixture set that
	// ran out after one would make every test that syncs twice brittle for no
	// reason a reader could see.
	n := min(t.used[k], len(found)-1)
	t.used[k]++
	t.served = append(t.served, found[n])
	return t.exchanges[found[n]].response(req), nil
}

// capture makes the request for real and writes down what came back.
func (t *Transport) capture(req *http.Request, body []byte) (*http.Response, error) {
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	answer, err := take(resp.Body)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(answer))

	e := Exchange{
		Request: Request{
			Method:  req.Method,
			URL:     t.cleanURL(req.URL),
			Headers: t.cleanHeaders(req.Header),
			Body:    payload(t.scrub(body)),
		},
		Response: Response{
			Status:  resp.StatusCode,
			Headers: t.cleanHeaders(resp.Header),
			Body:    payload(t.scrub(answer)),
		},
	}

	t.mu.Lock()
	t.exchanges = append(t.exchanges, e)
	t.mu.Unlock()
	return resp, nil
}

// Exchanges returns what has been recorded so far.
func (t *Transport) Exchanges() []Exchange {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.exchanges)
}

// Unused returns the recorded requests nothing asked for.
//
// A fixture set drifts the same way a comment does. A connector that stopped
// calling an endpoint leaves the recording of it behind, and the next person to
// read the directory takes it for a description of what the connector does.
// This is how a test says so.
func (t *Transport) Unused() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for i, e := range t.exchanges {
		if slices.Contains(t.served, i) {
			continue
		}
		u, err := url.Parse(e.Request.URL)
		if err != nil {
			out = append(out, e.Request.Method+" "+e.Request.URL)
			continue
		}
		out = append(out, t.key(e.Request.Method, u, e.Request.Body.Bytes()))
	}
	return out
}

// summary is what the recording holds, for the error a missing one produces.
func (t *Transport) summary() string {
	seen := make(map[string]bool, len(t.exchanges))
	var keys []string
	for _, e := range t.exchanges {
		u, err := url.Parse(e.Request.URL)
		if err != nil {
			continue
		}
		k := t.key(e.Request.Method, u, e.Request.Body.Bytes())
		if !seen[k] {
			seen[k] = true
			keys = append(keys, "  "+k)
		}
	}
	if len(keys) == 0 {
		return "  nothing at all"
	}
	slices.Sort(keys)
	return strings.Join(keys, "\n")
}

// key is what two requests have to agree on to be the same request.
//
// The scheme and the host are left out, and that is deliberate. A recording is
// a description of an API rather than of the workspace it was taken from, and
// requiring a test to point its client at the hostname somebody happened to
// record against would make every fixture set carry a company's subdomain
// around with it.
//
// The redacted parameters are left out for the same reason one step further in:
// a recording made with a token has to answer a test that has none.
func (t *Transport) key(method string, u *url.URL, body []byte) string {
	var b strings.Builder
	b.WriteString(strings.ToUpper(method))
	b.WriteByte(' ')
	b.WriteString(u.EscapedPath())
	if q := t.cleanQuery(u.Query()); len(q) > 0 {
		b.WriteByte('?')
		b.WriteString(q.Encode())
	}
	if len(body) > 0 {
		// A body is part of the question for the sources that ask with a form
		// post rather than a query string, and several of them do.
		b.WriteByte(' ')
		b.WriteString(canonicalBody(body))
	}
	return b.String()
}

// cleanURL is a URL with its credentials taken out, for writing down.
func (t *Transport) cleanURL(u *url.URL) string {
	clean := *u
	clean.User = nil
	if q := t.cleanQuery(u.Query()); len(q) > 0 {
		clean.RawQuery = q.Encode()
	} else {
		clean.RawQuery = ""
	}
	return clean.String()
}

func (t *Transport) cleanQuery(q url.Values) url.Values {
	for name := range q {
		if t.redactedParam(name) {
			q.Del(name)
		}
	}
	return q
}

func (t *Transport) redactedParam(name string) bool {
	for _, p := range t.params {
		if strings.EqualFold(p, name) {
			return true
		}
	}
	return false
}

// cleanHeaders is a header set with the credentials replaced.
func (t *Transport) cleanHeaders(h http.Header) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for name, values := range h {
		if t.redactedHeader(name) {
			out[http.CanonicalHeaderKey(name)] = []string{Redacted}
			continue
		}
		out[http.CanonicalHeaderKey(name)] = slices.Clone(values)
	}
	return out
}

func (t *Transport) redactedHeader(name string) bool {
	for _, h := range t.headers {
		if strings.EqualFold(h, name) {
			return true
		}
	}
	return false
}

// canonicalBody is a body reduced to something two identical requests agree on.
//
// A form post is compared field by field rather than byte by byte, because the
// order a client writes its fields in is not part of what it asked. Anything
// else is compared as it stands.
func canonicalBody(body []byte) string {
	if v, err := url.ParseQuery(string(body)); err == nil && looksLikeForm(body) {
		return v.Encode()
	}
	return strings.Join(strings.Fields(string(body)), " ")
}

// looksLikeForm reports whether a body is a URL encoded form.
//
// url.ParseQuery accepts almost anything, including a line of prose, so the
// shape has to be checked rather than the parse.
func looksLikeForm(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "=") && !strings.ContainsAny(s, "{}\n\r")
}

// take reads a body and closes it, and returns nil for one that was not there.
func take(r io.ReadCloser) ([]byte, error) {
	if r == nil || r == http.NoBody {
		return nil, nil
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, nil
	}
	return b, nil
}

// Exchange is one request and the answer it got.
type Exchange struct {
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}

// Request is what was asked.
type Request struct {
	Method string `json:"method"`
	URL    string `json:"url"`

	// Headers are kept for the reader rather than for the matching. Nothing is
	// compared against them, because a client library that adds an Accept
	// header this year and not last year has not changed the question it is
	// asking.
	Headers map[string][]string `json:"headers,omitempty"`

	Body Payload `json:"body,omitzero"`
}

// Response is what came back.
type Response struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    Payload             `json:"body,omitzero"`
}

// response turns a recorded answer back into one a client can read.
func (e Exchange) response(req *http.Request) *http.Response {
	body := e.Response.Body.Bytes()
	header := make(http.Header, len(e.Response.Headers))
	for name, values := range e.Response.Headers {
		header[http.CanonicalHeaderKey(name)] = slices.Clone(values)
	}
	status := e.Response.Status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// Payload is the body of a request or a response, written down in whichever of
// three forms keeps the file readable.
//
// A recording is reviewed, and a review of a wall of base64 is not a review.
// A JSON body is nested into the file as JSON, so a change to one field of one
// response shows up in a diff as a change to one line. Text that is not JSON is
// a string. Anything that is neither is bytes, and only then.
type Payload struct {
	JSON   json.RawMessage `json:"json,omitempty"`
	Text   string          `json:"text,omitempty"`
	Binary []byte          `json:"binary,omitempty"`
}

// Bytes is the payload as it went over the wire.
func (p Payload) Bytes() []byte {
	switch {
	case len(p.JSON) > 0:
		return p.JSON
	case p.Text != "":
		return []byte(p.Text)
	default:
		return p.Binary
	}
}

// payload picks the form to write a body down in.
func payload(body []byte) Payload {
	switch {
	case len(body) == 0:
		return Payload{}
	case json.Valid(body):
		// The file is written with indentation, which reformats this along with
		// everything around it. That changes the bytes a replay hands back and
		// it is worth saying out loud: what is being recorded is the document
		// the service sent rather than its whitespace, and the alternative is a
		// wall of one line responses that no diff can be read against.
		return Payload{JSON: body}
	case utf8.Valid(body):
		return Payload{Text: string(body)}
	default:
		return Payload{Binary: body}
	}
}
