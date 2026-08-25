package api

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// This file is the adversarial half of the permission tests, and it is inside
// the package for the reason surface_test.go is: it walks the route table, so a
// surface added next year is attacked without anybody remembering to come back
// here.
//
// The tests elsewhere are written from the product's side. This document is
// readable by this person, therefore it comes back. These are written from the
// other side, by somebody who has the corpus and wants to know what is in the
// part of it they were not given. The corpus is random, the permissions on it
// are random, and what is asserted is not that the right documents came back
// but that nothing in the response depended on the ones that did not.
//
// The oracle is a second server over a second store holding only what the
// reader may read. If the rest of the corpus has no effect, the two servers
// answer the same bytes, and every difference between them is a channel: a
// total, a facet count, a spelling correction, an ordering that moved because a
// document nobody may read was scored against.
//
// # What this deliberately does not assert
//
// Response timing. A test that measures a millisecond on a shared runner
// measures the runner. The work based counters under benchcorpus are where a
// permission path that walks the corpus for one reader and not another shows
// up, and they fail on the count rather than on the clock.
//
// The size of the corpus. GET /api/v1/stats reports how many documents the
// deployment holds, and it reports the same number to everybody. That is a
// decision rather than a leak this suite missed: the number names no document,
// no title, no source and no container, and an operator needs it to tell a
// finished sync from a stalled one.

// worlds is how many corpora the property tests run over by default.
//
// The seeds are fixed rather than drawn from a clock. A suite that fails on
// Tuesday and passes on Wednesday teaches people to run it again instead of
// reading it, and a failure here is a leak that somebody has to be able to
// reproduce on their own machine from the seed in the message.
const worlds = 8

// deeperSweep names the environment variable that widens the sweep, which is
// what CI sets. Eight corpora on a laptop keeps the package fast, and a
// hundred on every change is what turns a rare shape into a caught one.
const deeperSweep = "GENBA_PERMISSION_SEEDS"

// The reader every attack below is made as.
const (
	readerTenant  = "acme"
	readerSubject = "u_mei"
)

// readerHeaders is that reader on the wire.
func readerHeaders() map[string]string {
	return map[string]string{
		HeaderTenant:     readerTenant,
		HeaderSubject:    readerSubject,
		HeaderGroups:     "gdrive:eng@acme.com,slack:eng,jira:eng",
		HeaderIdentities: "gdrive:mei@acme.com,slack:U04MEI,jira:mei",
	}
}

// theReader is the same principal the server will build, built by the same
// code.
//
// It matters that this is not written out by hand. The list of who may read
// what is worked out here and compared against what the server served, so a
// second reading of the headers would be a second definition of who is asking,
// and the two would agree right up until one of them was wrong.
func theReader(t *testing.T) *acl.Principal {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	for k, v := range readerHeaders() {
		r.Header.Set(k, v)
	}
	p, err := HeaderAuth{Tenant: readerTenant}.Authenticate(r)
	if err != nil {
		t.Fatalf("building the reader: %v", err)
	}
	return p
}

// world is a random corpus split into what the reader may read and what they
// may not.
type world struct {
	seed    uint64
	all     []doc.Document
	visible []doc.Document
	hidden  []doc.Document
}

// newWorld generates one corpus and works out, independently of the server,
// which of it the reader may read.
func newWorld(t *testing.T, seed uint64) *world {
	t.Helper()
	rnd := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	p, png := theReader(t), pixels(t)

	w := &world{seed: seed}
	for i := range 14 + rnd.IntN(18) {
		d := document(rnd, i, png)
		w.all = append(w.all, d)
		if d.Permissions.Allows(p) {
			w.visible = append(w.visible, d)
			continue
		}
		w.hidden = append(w.hidden, d)
	}
	if len(w.visible) == 0 || len(w.hidden) == 0 {
		t.Fatalf("seed %d made %d readable and %d unreadable documents, and a corpus with none of either proves nothing",
			seed, len(w.visible), len(w.hidden))
	}
	return w
}

// The words the corpus is written out of. They are shared on purpose: every
// document is about the same handful of subjects, so a query matches across the
// permission boundary rather than neatly on one side of it.
var (
	worldSources    = []string{"gdrive", "slack", "jira"}
	worldKinds      = []doc.Kind{doc.KindPage, doc.KindMessage, doc.KindTicket, doc.KindFile, doc.KindCode}
	worldContainers = []string{"handbook", "platform", "payments", "hiring"}
	worldPeople     = []doc.Person{
		{Subject: "u_mei", Name: "Mei", Email: "mei@acme.com"},
		{Subject: "u_ravi", Name: "Ravi", Email: "ravi@acme.com"},
		{Subject: "u_juno", Name: "Juno", Email: "juno@acme.com"},
	}
)

// worldStart is when the corpus was written, so that a recency prior is the
// same number on both servers.
var worldStart = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// document is one random document, marked with two words that appear nowhere
// else in the corpus.
//
// One of them is in the title and is the word an attacker searches for. The
// other is only ever in the body, and it is what the responses are scanned for,
// because a response that echoes the query back is not leaking anything and a
// test that could not tell those apart would be reporting the echo.
func document(rnd *rand.Rand, i int, png []byte) doc.Document {
	nonce := fmt.Sprintf("kx%04d", i)
	source := worldSources[rnd.IntN(len(worldSources))]
	container := worldContainers[rnd.IntN(len(worldContainers))]
	author := worldPeople[rnd.IntN(len(worldPeople))]

	d := doc.Document{
		ID:        "doc-" + nonce,
		Tenant:    readerTenant,
		Source:    source,
		Kind:      worldKinds[rnd.IntN(len(worldKinds))],
		Title:     "Runbook " + nonce + " for " + container,
		Author:    author,
		Owner:     author,
		Container: container,
		URL:       "https://example.com/d/" + nonce,
		Body: "The " + nonce + " runbook covers the " + container + " rotation. " +
			"Escalate to the " + container + " on call when the queue backs up. " +
			"The change window is recorded under " + "zzq" + fmt.Sprintf("%04d", i) + " every quarter.",
		ModifiedAt:  worldStart.Add(-time.Duration(i) * time.Hour),
		CreatedAt:   worldStart.Add(-time.Duration(i+100) * time.Hour),
		Permissions: permissionsFor(rnd, i, source, author),
	}
	// Every fifth document is an image with real bytes behind it, so that the
	// two surfaces that answer with something other than JSON are attacked with
	// a document they would otherwise have served.
	if i%5 == 1 {
		d.Kind = doc.KindImage
		d.Properties = map[string]string{doc.MediaType: "image/png"}
		d.Content = &doc.Content{Bytes: png, Width: 8, Height: 8}
	}
	return d
}

// permissionsFor is a random rule, drawn from the shapes real sources produce.
//
// The first three documents are fixed rather than drawn, because every seed has
// to give the suite something readable, something unreadable, and one of each
// with bytes behind it. Everything after them is the draw.
func permissionsFor(rnd *rand.Rand, i int, source string, author doc.Person) acl.Permissions {
	perm := acl.Permissions{Mode: acl.ModeACL, Source: source, Version: 1}
	perm.Owner = acl.Ref{Source: source, Value: identityIn(source, author)}

	shape := rnd.IntN(10)
	switch i {
	case 0:
		shape = 0 // readable by everyone in the tenant
	case 1:
		shape = 8 // an image only its owner may read, and the owner is not the reader
	case 2:
		shape = 9 // a rule that never resolved
	}

	mine := acl.Ref{Source: source, Value: readerIdentity(source)}
	theirs := acl.Ref{Source: source, Value: "ravi@acme.com"}
	myGroup := acl.Ref{Source: source, Value: readerGroup(source)}
	theirGroup := acl.Ref{Source: source, Value: "finance@acme.com"}

	switch shape {
	case 0:
		perm.Mode = acl.ModePublicToTenant
	case 1, 2:
		perm.AllowGroups = []acl.Ref{myGroup, theirGroup}
	case 3:
		perm.AllowGroups = []acl.Ref{theirGroup}
	case 4:
		perm.AllowUsers = []acl.Ref{mine}
	case 5:
		perm.AllowUsers = []acl.Ref{theirs}
	case 6:
		// Named on the allow list and denied by name, which is the case that
		// tells a rule that reads its clauses in order from one that does not.
		perm.AllowGroups = []acl.Ref{myGroup}
		perm.AllowUsers = []acl.Ref{mine}
		perm.DenyUsers = []acl.Ref{mine}
	case 7:
		perm.Mode = acl.ModeOwnerOnly
		perm.Owner = mine
	case 8:
		perm.Mode = acl.ModeOwnerOnly
		perm.Owner = theirs
	case 9:
		perm.Mode = acl.ModeUnknown
		perm.Reason = "the group " + theirGroup.GroupKey() + " is not in the directory"
	}
	return perm
}

// readerIdentity and readerGroup are how the reader is named by each source,
// which is what a rule from that source compares against.
func readerIdentity(source string) string {
	switch source {
	case "slack":
		return "U04MEI"
	case "jira":
		return "mei"
	default:
		return "mei@acme.com"
	}
}

func readerGroup(source string) string {
	if source == "gdrive" {
		return "eng@acme.com"
	}
	return "eng"
}

// identityIn is how one of the made up people is named by a source.
func identityIn(source string, p doc.Person) string {
	if p.Subject == readerSubject {
		return readerIdentity(source)
	}
	return p.Email
}

// nonceOf is the word in a document's title that nothing else in the corpus
// has, and secretOf is the one in its body.
func nonceOf(d doc.Document) string  { return strings.TrimPrefix(d.ID, "doc-") }
func secretOf(d doc.Document) string { return "zzq" + strings.TrimPrefix(nonceOf(d), "kx") }

// serving is a server over the given documents.
func (w *world) serving(t *testing.T, docs []doc.Document) http.Handler {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })
	if len(docs) > 0 {
		if err := st.Put(t.Context(), docs...); err != nil {
			t.Fatalf("seeding the corpus: %v", err)
		}
	}
	s := New(st, index.New(st), HeaderAuth{Tenant: readerTenant})
	t.Cleanup(func() { _ = s.Close() })
	return s.Handler()
}

// full is the deployment, and readable is the same deployment in a universe
// where the documents this reader may not read were never indexed.
func (w *world) full(t *testing.T) http.Handler     { return w.serving(t, w.all) }
func (w *world) readable(t *testing.T) http.Handler { return w.serving(t, w.visible) }

// queries is what somebody who has seen the corpus would type to find the part
// of it they were not given.
//
// Every one of them is aimed at a document the reader may not read: its own
// rare word, its title, a sentence out of its body, and each of the operators
// that narrow to the source, the space, the type and the author it has.
func (w *world) queries() []string {
	out := []string{"runbook", "runbook rotation", "escalate queue", "sort:recent runbook"}
	for _, d := range w.hidden {
		out = append(out,
			nonceOf(d),
			d.Title,
			"escalate to the "+d.Container+" on call",
			"app:"+d.Source+" runbook",
			"in:"+d.Container+" runbook",
			"type:"+string(d.Kind)+" runbook",
			"from:"+d.Author.Name+" runbook",
			"owner:"+d.Owner.Email+" runbook",
		)
	}
	return out
}

// TestASearchIsWhatItWouldBeIfTheHiddenDocumentsWereNotThere compares a search
// against the whole corpus with the same search against only the part of it the
// reader may read.
//
// This is the one that catches the counting. A facet count, a total, a spelling
// correction and an ordering are all numbers the reader is allowed to see, and
// each of them is derived from a set of documents, so each of them is a way of
// asking how many there are and roughly what they say. Comparing whole
// responses rather than any one of those fields means the next number added to
// this endpoint is covered on the day it is added.
func TestASearchIsWhatItWouldBeIfTheHiddenDocumentsWereNotThere(t *testing.T) {
	for _, seed := range seeds(t) {
		w := newWorld(t, seed)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			full, readable := w.full(t), w.readable(t)
			for _, q := range w.queries() {
				target := "/api/v1/search?q=" + url.QueryEscape(q)
				_, got := askAs(t, full, http.MethodGet, target)
				_, want := askAs(t, readable, http.MethodGet, target)
				if diff := differs(t, got, want); diff != "" {
					t.Errorf("searching %q over the whole corpus is not what it is over the readable part of it, which is a channel:\n%s", q, diff)
				}
			}
		})
	}
}

// TestAForbiddenDocumentAnswersLikeAMissingOne holds every surface that takes
// an id to the same answer for a document the reader may not read and for one
// that was never there.
//
// A 403 beside a 404 is a lookup service for document ids: paste in the id from
// a link somebody forwarded, and the difference between the two tells you
// whether it exists. The bodies are compared as bytes rather than as statuses
// because the sentence inside is the other half of the same tell.
func TestAForbiddenDocumentAnswersLikeAMissingOne(t *testing.T) {
	for _, seed := range seeds(t) {
		w := newWorld(t, seed)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			h := w.full(t)
			for _, probe := range idProbes() {
				missing := probe.at(t, h, "doc-kx9999")
				for _, d := range w.hidden {
					if forbidden := probe.at(t, h, d.ID); forbidden != missing {
						t.Errorf("%s answers one way for %s, which is there and may not be read, and another for a document that is not there:\n%s\n%s",
							probe.name, d.ID, forbidden, missing)
					}
				}
			}
		})
	}
}

// TestTheSameSurfacesAnswerForADocumentTheReaderMayRead is what stops the test
// above from passing on a server that answers 404 to everything.
func TestTheSameSurfacesAnswerForADocumentTheReaderMayRead(t *testing.T) {
	w := newWorld(t, 1)
	h := w.full(t)
	for _, probe := range idProbes() {
		missing := probe.at(t, h, "doc-kx9999")
		var answered bool
		for _, d := range w.visible {
			if probe.at(t, h, d.ID) != missing {
				answered = true
				break
			}
		}
		if !answered {
			t.Errorf("%s answers the same for every readable document as it does for one that is not there, so the test above says nothing about it", probe.name)
		}
	}
}

// TestTheAnswerQuotesOnlyWhatTheReaderCouldHaveOpened attacks the passages
// above the results with the documents they are drawn from.
//
// The quotes are the one place in the product where text from a document is
// shown outside the row that names it, and the query is somebody else's
// sentence, so this is the surface most likely to be got wrong by a change that
// looks like an improvement to the answer.
func TestTheAnswerQuotesOnlyWhatTheReaderCouldHaveOpened(t *testing.T) {
	for _, seed := range seeds(t) {
		w := newWorld(t, seed)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			h := w.full(t)
			readable := make(map[string]bool, len(w.visible))
			for _, d := range w.visible {
				readable[d.ID] = true
			}

			for _, d := range w.hidden {
				for _, q := range []string{d.Title, d.Body, nonceOf(d), "escalate to the " + d.Container + " on call"} {
					_, body := askAs(t, h, http.MethodGet, "/api/v1/search?q="+url.QueryEscape(q))
					var out struct {
						Answer *struct {
							Quotes []struct {
								ID   string `json:"id"`
								Text string `json:"text"`
							} `json:"quotes"`
						} `json:"answer"`
					}
					if err := json.Unmarshal([]byte(body), &out); err != nil {
						t.Fatalf("decoding the search: %v\n%s", err, body)
					}
					if out.Answer == nil {
						continue
					}
					for _, quoted := range out.Answer.Quotes {
						if !readable[quoted.ID] {
							t.Errorf("asking %q quoted %s, which this reader cannot open: %q", q, quoted.ID, quoted.Text)
						}
					}
				}
			}
		})
	}
}

// raids is how each surface that can serve content is aimed at the documents
// the reader may not read.
//
// One entry per content route. A route without one fails the walk rather than
// being skipped, which is the rule the other two walks over this table use: a
// surface added to the API has to say how somebody would attack it, in the
// change that adds it rather than in a review a year later.
var raids = map[string]func(w *world) []string{
	"GET /api/v1/search": func(w *world) []string {
		var out []string
		for _, q := range w.queries() {
			out = append(out, "/api/v1/search?q="+url.QueryEscape(q))
		}
		return out
	},
	"GET /api/v1/suggest": func(w *world) []string {
		var out []string
		for _, d := range w.hidden {
			nonce := nonceOf(d)
			out = append(out,
				"/api/v1/suggest?q="+url.QueryEscape(nonce),
				"/api/v1/suggest?q="+url.QueryEscape(nonce[:4]),
				"/api/v1/suggest?q="+url.QueryEscape(d.Title),
				"/api/v1/suggest?q="+url.QueryEscape("runbook"),
			)
		}
		return out
	},
	"GET /api/v1/documents": func(w *world) []string {
		query := make(url.Values)
		for _, d := range w.hidden {
			query.Add("id", d.ID)
		}
		return []string{"/api/v1/documents?" + query.Encode()}
	},
	"GET /api/v1/documents/{id}": func(w *world) []string {
		return targets(w, "/api/v1/documents/%s")
	},
	"GET /api/v1/documents/{id}/content": func(w *world) []string {
		return targets(w, "/api/v1/documents/%s/content")
	},
	"GET /api/v1/documents/{id}/thumbnail": func(w *world) []string {
		return targets(w, "/api/v1/documents/%s/thumbnail?size=96")
	},
	"GET /api/v1/reported": func(*world) []string {
		return []string{"/api/v1/reported", "/api/v1/reported?limit=100"}
	},
	"GET /api/v1/recent": func(*world) []string {
		return []string{"/api/v1/recent", "/api/v1/recent?limit=100"}
	},
}

// targets is one request per unreadable document.
func targets(w *world, pattern string) []string {
	out := make([]string, 0, len(w.hidden))
	for _, d := range w.hidden {
		out = append(out, fmt.Sprintf(pattern, url.PathEscape(d.ID)))
	}
	return out
}

// TestNoSurfaceNamesADocumentTheReaderCannotRead is the walk.
//
// It goes through the router the way a browser does, over a random corpus, and
// reads every response looking for the word that only one document has. What it
// is looking for is not a bug anybody would write on purpose. It is the second
// list on a screen, the preview that was added to a panel, the field somebody
// widened, and the way those get found is by attacking every surface with the
// same corpus rather than by reviewing each one when it lands.
func TestNoSurfaceNamesADocumentTheReaderCannotRead(t *testing.T) {
	for _, seed := range seeds(t) {
		w := newWorld(t, seed)
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			h := w.full(t)
			poison(t, h, w)

			for _, rt := range served(t) {
				pattern := rt.Method + " " + rt.Pattern
				build, ok := raids[pattern]
				if !ok {
					t.Errorf("%s can serve content and this walk does not know how to attack it, add it to raids in adversary_test.go", pattern)
					continue
				}
				for _, target := range build(w) {
					_, body := askAs(t, h, rt.Method, target)
					if named := leaked(w, body); len(named) != 0 {
						t.Errorf("%s served %v of a corpus this reader may not read:\n%s\n%s",
							pattern, named, target, body)
					}
				}
			}
		})
	}
}

// poison is the attacker putting the documents they may not read into the two
// screens that are built out of their own history.
//
// Recording an open and reporting a document are both writes that take an id
// and no permission of their own, which is right: refusing them would answer
// the question of whether the id exists. So the check has to be on the way out,
// on the screens that read the history back, and the way to find out whether it
// is there is to fill the history with ids that must never come back.
func poison(t *testing.T, h http.Handler, w *world) {
	t.Helper()
	for _, d := range w.hidden {
		sendAs(t, h, http.MethodPost, "/api/v1/recent", `{"id":`+strconv.Quote(d.ID)+`}`)
		sendAs(t, h, http.MethodPost, "/api/v1/documents/"+url.PathEscape(d.ID)+"/stale",
			`{"note":"this is a note about a document I cannot read"}`)
		sendAs(t, h, http.MethodPost, "/api/v1/documents/"+url.PathEscape(d.ID)+"/verify", `{}`)
	}
}

// leaked is which unreadable documents a response names.
//
// It looks for the id and for the word that is only in that document's body,
// and not for the word in its title, because the title word is what the query
// was made of and a response that echoes the query back is not telling anybody
// anything they did not type.
func leaked(w *world, body string) []string {
	var named []string
	for _, d := range w.hidden {
		if strings.Contains(body, d.ID) || strings.Contains(body, secretOf(d)) {
			named = append(named, d.ID)
		}
	}
	return named
}

// idProbe is one surface that takes a document id.
type idProbe struct {
	name   string
	method string
	path   string
	body   string
}

// idProbes is every surface that takes an id in its path, including the ones
// that write.
//
// The writes are here because a refusal is an answer too. Reporting a document
// that does not exist and reporting one that exists and is not yours have to
// come back the same, or the report button becomes the lookup service the read
// path was careful not to be.
func idProbes() []idProbe {
	return []idProbe{
		{name: "GET the document", method: http.MethodGet, path: "/api/v1/documents/%s"},
		{name: "GET the content", method: http.MethodGet, path: "/api/v1/documents/%s/content"},
		{name: "GET the thumbnail", method: http.MethodGet, path: "/api/v1/documents/%s/thumbnail?size=96"},
		{name: "report it as stale", method: http.MethodPost, path: "/api/v1/documents/%s/stale", body: `{"note":"the failover section is out of date"}`},
		{name: "withdraw that report", method: http.MethodDelete, path: "/api/v1/documents/%s/stale/mine"},
		{name: "vouch for it", method: http.MethodPost, path: "/api/v1/documents/%s/verify", body: `{}`},
		{name: "withdraw that", method: http.MethodDelete, path: "/api/v1/documents/%s/verify"},
		// A body the endpoint accepts, because a body it rejects is answered
		// before the id is looked at and would make this probe assert nothing.
		{name: "correct its owner", method: http.MethodPut, path: "/api/v1/documents/%s/owner", body: `{"email":"mei@acme.com","name":"Mei"}`},
		{name: "withdraw that correction", method: http.MethodDelete, path: "/api/v1/documents/%s/owner"},
	}
}

// reply is what one probe came back with, and it is comparable on purpose:
// what this file asserts about two of them is that they are the same value.
type reply struct {
	code        int
	contentType string
	body        string
}

func (r reply) String() string {
	return fmt.Sprintf("%d %s %s", r.code, r.contentType, strings.TrimSpace(r.body))
}

// at asks this probe about one id.
func (p idProbe) at(t *testing.T, h http.Handler, id string) reply {
	t.Helper()
	w := record(t, h, p.method, fmt.Sprintf(p.path, url.PathEscape(id)), p.body)
	return reply{code: w.Code, contentType: w.Header().Get("Content-Type"), body: w.Body.String()}
}

// seeds is which corpora to attack.
func seeds(t *testing.T) []uint64 {
	t.Helper()
	n := worlds
	if raw := strings.TrimSpace(os.Getenv(deeperSweep)); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("%s is %q, which is not a number of corpora", deeperSweep, raw)
		}
		n = parsed
	}
	out := make([]uint64, 0, n)
	for i := range n {
		out = append(out, uint64(i)+1)
	}
	return out
}

// askAs makes one request as the reader.
func askAs(t *testing.T, h http.Handler, method, target string) (code int, body string) {
	t.Helper()
	w := record(t, h, method, target, "")
	return w.Code, w.Body.String()
}

// sendAs is askAs with a body, for the writes.
func sendAs(t *testing.T, h http.Handler, method, target, body string) (code int, out string) {
	t.Helper()
	w := record(t, h, method, target, body)
	return w.Code, w.Body.String()
}

func record(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(t.Context(), method, target, nil)
	} else {
		r = httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range readerHeaders() {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// differs compares two responses field by field and says where they part,
// ignoring how long each one took.
//
// Field by field rather than as two blocks of JSON, because these responses run
// to a few hundred lines and a failure that prints both of them in full is a
// failure somebody diffs by hand at the point where they most want to be told
// the answer. What a leak looks like here is one number.
func differs(t *testing.T, got, want string) string {
	t.Helper()
	found := apart("", decodeJSON(t, got), decodeJSON(t, want))
	if len(found) == 0 {
		return ""
	}
	return strings.Join(found, "\n")
}

// apart walks two decoded responses together and returns every field they
// disagree on, named by its path.
func apart(path string, got, want any) []string {
	switch a := got.(type) {
	case map[string]any:
		b, ok := want.(map[string]any)
		if !ok {
			break
		}
		var found []string
		for _, key := range union(a, b) {
			if path == "" && key == "took_ms" {
				continue
			}
			left, inLeft := a[key]
			right, inRight := b[key]
			switch {
			case !inLeft:
				found = append(found, at(path, key)+" is only there over the readable part of the corpus")
			case !inRight:
				found = append(found, at(path, key)+" is only there over the whole corpus")
			default:
				found = append(found, apart(at(path, key), left, right)...)
			}
		}
		return found
	case []any:
		b, ok := want.([]any)
		if !ok {
			break
		}
		if len(a) != len(b) {
			return []string{fmt.Sprintf("%s has %d over the whole corpus and %d over the readable part of it", path, len(a), len(b))}
		}
		var found []string
		for i := range a {
			found = append(found, apart(fmt.Sprintf("%s[%d]", path, i), a[i], b[i])...)
		}
		return found
	}
	if reflect.DeepEqual(got, want) {
		return nil
	}
	return []string{fmt.Sprintf("%s is %v over the whole corpus and %v over the readable part of it", path, got, want)}
}

// union is every key in either response, in a stable order.
func union(a, b map[string]any) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

func at(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func decodeJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decoding a response: %v\n%s", err, body)
	}
	return out
}
