package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/audit"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// This file is inside the package because it walks the route table, which is
// not exported and should not be. What it enforces is the promise the product
// makes about its audit trail: that the set of surfaces which can put a
// document in front of somebody and the set which write a record are the same
// set, and that they stay the same set as the API grows.

// requests is how to exercise each surface that serves content.
//
// Every route the table marks as content needs one, and a route without one
// fails the walk rather than being skipped. That is the point: adding an
// endpoint that returns documents means answering the question of what its
// audit record looks like, in the same change rather than a year later.
var requests = map[string]string{
	"GET /api/v1/search":                   "/api/v1/search?q=payments",
	"GET /api/v1/suggest":                  "/api/v1/suggest?q=pay",
	"GET /api/v1/documents":                "/api/v1/documents?id=img",
	"GET /api/v1/documents/{id}":           "/api/v1/documents/img",
	"GET /api/v1/documents/{id}/content":   "/api/v1/documents/img/content",
	"GET /api/v1/documents/{id}/thumbnail": "/api/v1/documents/img/thumbnail?size=96",
	"GET /api/v1/reported":                 "/api/v1/reported",
	"GET /api/v1/recent":                   "/api/v1/recent",
}

// quiet is every other route, with the reason it serves no content.
//
// It is spelled out one route at a time on purpose. A new endpoint is in
// neither list, so somebody has to decide which it is, and writing "it returns
// no documents" next to a route that returns documents is a thing a reviewer
// can see.
var quiet = map[string]string{
	"GET /healthz":                                 "liveness, and it takes no principal",
	"GET /readyz":                                  "readiness, and it takes no principal",
	"GET /api/v1/me":                               "who the caller is, which they already know",
	"POST /api/v1/documents/{id}/verify":           "a claim about a document, not the document",
	"DELETE /api/v1/documents/{id}/verify":         "withdrawing a claim",
	"PUT /api/v1/documents/{id}/owner":             "a correction to who is accountable",
	"DELETE /api/v1/documents/{id}/owner":          "withdrawing that correction",
	"POST /api/v1/documents/{id}/stale":            "a report about a document, not the document",
	"DELETE /api/v1/documents/{id}/stale":          "clearing a report",
	"DELETE /api/v1/documents/{id}/stale/mine":     "withdrawing one's own report",
	"POST /api/v1/recent":                          "recording that something was opened, and it answers 204",
	"GET /api/v1/stats":                            "counts of the corpus, with nothing of any document in them",
	"GET /api/v1/events":                           "index events, which carry no document",
	"GET /api/v1/admin/answers":                    "curated answers, which are written here rather than read from the corpus",
	"PUT /api/v1/admin/answers/{id}":               "writing one",
	"DELETE /api/v1/admin/answers/{id}":            "retracting one",
	"GET /api/v1/admin/operations":                 "what the connectors are doing",
	"GET /api/v1/admin/access":                     "why one principal can see one document, which is a decision rather than content",
	"POST /api/v1/admin/connectors":                "configuration",
	"DELETE /api/v1/admin/connectors/{source}":     "configuration",
	"POST /api/v1/admin/connectors/{source}/start": "configuration",
	"POST /api/v1/admin/connectors/{source}/stop":  "configuration",
	"POST /api/v1/admin/connectors/{source}/sync":  "configuration",
}

// TestEverySurfaceThatServesContentWritesARecord is the walk.
//
// It goes through the router the way a browser does, so what it proves is not
// that a handler calls a function but that a request arriving at this server
// leaves a record behind, with the caller on it and the route it came through.
func TestEverySurfaceThatServesContentWritesARecord(t *testing.T) {
	for _, rt := range served(t) {
		pattern := rt.Method + " " + rt.Pattern
		t.Run(pattern, func(t *testing.T) {
			target, ok := requests[pattern]
			if !ok {
				t.Fatalf("%s serves content and this test does not know how to call it, add it to requests in surface_test.go", pattern)
			}

			wk := auditing(t)
			wk.ask(t, rt.Method, target)

			got := wk.records()
			if len(got) != 1 {
				t.Fatalf("%s wrote %d records, want exactly one", pattern, len(got))
			}
			rec := got[0]
			switch {
			case rec.Outcome != audit.Served:
				t.Errorf("the record says %q, want a served access", rec.Outcome)
			case rec.Surface != pattern:
				t.Errorf("the record came through %q, want %q", rec.Surface, pattern)
			case rec.Subject != "u_mei" || rec.Tenant != "acme":
				t.Errorf("the record names %q of %q", rec.Subject, rec.Tenant)
			case rec.Kind != "user":
				t.Errorf("the record calls the caller a %q", rec.Kind)
			case rec.At.IsZero():
				t.Error("the record is not stamped")
			}
		})
	}
}

// TestARecordNamesNoContentAndNoGroup walks the same surfaces and reads what
// was written.
//
// An audit trail is kept for years and is read by more people than the corpus
// is, so a title in it is a leak with a long tail and a group name in it
// describes the shape of the organisation to everybody holding the export.
func TestARecordNamesNoContentAndNoGroup(t *testing.T) {
	wk := auditing(t)
	for _, rt := range served(t) {
		if target, ok := requests[rt.Method+" "+rt.Pattern]; ok {
			wk.ask(t, rt.Method, target)
		}
	}

	// The title of a document in the corpus, the body of one, and the group that
	// admitted the caller to both.
	secrets := []string{"Payments failover runbook", "Fail the payments queue", "architecture.png", "eng@acme.com"}
	for _, rec := range wk.records() {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, secret := range secrets {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("a record of %s carries %q:\n%s", rec.Surface, secret, raw)
			}
		}
	}
}

// TestEveryRouteHasSaidWhetherItServesContent is the other half of the walk.
//
// The walk above can only hold routes to a promise they have made, so this is
// what stops a new endpoint from quietly making none.
func TestEveryRouteHasSaidWhetherItServesContent(t *testing.T) {
	s := New(corpus(t), nil, HeaderAuth{Tenant: "acme"})
	t.Cleanup(func() { _ = s.Close() })

	for _, rt := range s.routes() {
		pattern := rt.Method + " " + rt.Pattern
		if rt.Content {
			if _, ok := quiet[pattern]; ok {
				t.Errorf("%s is marked as serving content and is also listed as serving none", pattern)
			}
			continue
		}
		if _, ok := quiet[pattern]; !ok {
			t.Errorf("%s serves no content according to the route table, say why in quiet in surface_test.go, or mark the route as content", pattern)
		}
	}
}

// TestAuditingIsOnWithoutBeingConfigured. There is no option that turns this
// off, and the way that is proved is that a server built with nothing but its
// dependencies still writes to something.
func TestAuditingIsOnWithoutBeingConfigured(t *testing.T) {
	s := New(corpus(t), nil, HeaderAuth{Tenant: "acme"})
	t.Cleanup(func() { _ = s.Close() })

	if s.audit == nil {
		t.Fatal("a server built with no options has nowhere to write its audit records")
	}
	s.audit.Write(audit.Record{Action: audit.Read, Outcome: audit.Served})
	if err := s.audit.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if st := s.audit.Stats(); st.Written != 1 {
		t.Errorf("the default log wrote %d records, want 1", st.Written)
	}
}

// served is the routes that can put a document in front of somebody.
func served(t *testing.T) []route {
	t.Helper()
	s := New(corpus(t), nil, HeaderAuth{Tenant: "acme"})
	t.Cleanup(func() { _ = s.Close() })

	var out []route
	for _, rt := range s.routes() {
		if rt.Content {
			out = append(out, rt)
		}
	}
	if len(out) == 0 {
		t.Fatal("no route says it serves content, which cannot be right")
	}
	return out
}

// walker is a server over the corpus with somewhere to read its records back
// from.
type walker struct {
	sink    *recorder
	log     *audit.Log
	handler http.Handler
}

func auditing(t *testing.T) *walker {
	t.Helper()
	sink := &recorder{}
	log := audit.New(sink)
	t.Cleanup(func() { _ = log.Close() })

	st := corpus(t)
	s := New(st, index.New(st), HeaderAuth{Tenant: "acme"}, WithAudit(log))
	t.Cleanup(func() { _ = s.Close() })
	return &walker{sink: sink, log: log, handler: s.Handler()}
}

// ask makes one request as somebody who may read the corpus, and waits for the
// records it produced to reach the sink.
func (wk *walker) ask(t *testing.T, method, target string) {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	r.Header.Set(HeaderSubject, "u_mei")
	r.Header.Set(HeaderGroups, "gdrive:eng@acme.com")
	w := httptest.NewRecorder()
	wk.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s answered %d, and this walk is about the paths that answer with content:\n%s",
			method, target, w.Code, w.Body.String())
	}
	// The write path is a queue, so a test that read the sink without this would
	// be asserting on how fast a goroutine was scheduled.
	if err := wk.log.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
}

func (wk *walker) records() []audit.Record { return wk.sink.records() }

// recorder is a sink that keeps what it was given.
type recorder struct {
	mu   sync.Mutex
	held []audit.Record
}

func (s *recorder) Append(rec audit.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held = append(s.held, rec)
	return nil
}

func (s *recorder) Flush() error { return nil }
func (s *recorder) Close() error { return nil }

func (s *recorder) records() []audit.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]audit.Record(nil), s.held...)
}

// corpus is one page and one image, both readable by the engineer the walk
// authenticates as.
func corpus(t *testing.T) *memstore.Store {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	perm := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
		Version:     1,
	}
	docs := []doc.Document{
		{
			ID: "d1", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Payments failover runbook", Body: "Fail the payments queue over to the replica.",
			// Written by the person the walks authenticate as, which is what puts
			// it on the screen that lists what readers have said about one's own
			// documents. Without an author that screen is empty for everybody and
			// a walk over it asserts nothing.
			Author:      doc.Person{Subject: "u_mei", Name: "Mei Tan"},
			Permissions: perm,
		},
		{
			ID: "img", Tenant: "acme", Source: "gdrive", Kind: doc.KindImage,
			Title: "architecture.png", Permissions: perm,
			Properties: map[string]string{doc.MediaType: "image/png"},
			Content:    &doc.Content{Bytes: pixels(t), Width: 8, Height: 8},
		},
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return st
}

// pixels is a real PNG, because the thumbnail endpoint decodes what it is
// given and a fake one would take that surface off the walk.
func pixels(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for x := range 8 {
		for y := range 8 {
			img.Set(x, y, color.NRGBA{R: 0x20, G: 0x80, B: 0xd0, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encoding a test image: %v", err)
	}
	return buf.Bytes()
}
