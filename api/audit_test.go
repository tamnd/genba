package api_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"sync"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/audit"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store/memstore"
)

// kept is a sink that holds what it was given, which is the only way to assert
// on a record without going through a file.
type kept struct {
	mu      sync.Mutex
	records []audit.Record
}

func (k *kept) Append(rec audit.Record) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.records = append(k.records, rec)
	return nil
}

func (k *kept) Flush() error { return nil }
func (k *kept) Close() error { return nil }

// trailed is a server over a small corpus, with its audit records readable.
func trailed(t *testing.T) (*kept, *audit.Log, http.Handler) {
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
			Title: "Payments failover runbook", Body: "Fail the payments queue over.",
			Permissions: perm,
		},
		{
			ID: "secret", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "Board pack", Body: "Numbers.",
			Permissions: acl.Permissions{
				Mode:        acl.ModeACL,
				Source:      "gdrive",
				AllowGroups: []acl.Ref{{Source: "gdrive", Value: "board@acme.com"}},
				Version:     1,
			},
		},
		{
			ID: "img", Tenant: "acme", Source: "gdrive", Kind: doc.KindImage,
			Title: "diagram.png", Permissions: perm,
			Properties: map[string]string{doc.MediaType: "image/png"},
			Content:    &doc.Content{Bytes: square(t), Width: 8, Height: 8},
		},
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	sink := &kept{}
	log := audit.New(sink)
	t.Cleanup(func() { _ = log.Close() })

	s := api.New(st, index.New(st), api.HeaderAuth{Tenant: "acme"}, api.WithAudit(log))
	t.Cleanup(func() { _ = s.Close() })
	return sink, log, s.Handler()
}

// written is the records one request produced.
func written(t *testing.T, sink *kept, log *audit.Log, h http.Handler, target string) []audit.Record {
	t.Helper()
	request(t, h, http.MethodGet, target, engineer())
	if err := log.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return append([]audit.Record(nil), sink.records...)
}

// TestADocumentSomebodyMayNotReadIsOnTheTrailAsARefusal, which is the record an
// investigation is most often actually looking for.
//
// It is the one a system that only logged successes would not have. The id is
// on it because that is what was asked for, and nothing else is, because the
// answer to the request was that there is nothing to say.
func TestADocumentSomebodyMayNotReadIsOnTheTrailAsARefusal(t *testing.T) {
	sink, log, h := trailed(t)

	got := written(t, sink, log, h, "/api/v1/documents/secret")
	if len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
	rec := got[0]
	if rec.Outcome != audit.Refused || rec.Action != audit.Read {
		t.Errorf("the record is a %q %q", rec.Outcome, rec.Action)
	}
	if len(rec.Documents) != 1 || rec.Documents[0].ID != "secret" {
		t.Errorf("the record names %v", rec.Documents)
	}
	if rec.Rule != "" {
		t.Errorf("a refusal carries the rule %q, and the trail does not say why somebody was turned away", rec.Rule)
	}
}

// TestADocumentThatIsNotThereLooksTheSameOnTheTrail as one somebody may not
// read.
//
// The API answers a missing document and a forbidden one identically, and so
// does the record. A trail that separated them would prove a document exists to
// anybody who can read the trail, which is more people than can read the
// document.
func TestADocumentThatIsNotThereLooksTheSameOnTheTrail(t *testing.T) {
	sink, log, h := trailed(t)

	missing := written(t, sink, log, h, "/api/v1/documents/nope")
	if len(missing) != 1 {
		t.Fatalf("%d records, want 1", len(missing))
	}
	if missing[0].Outcome != audit.Refused {
		t.Fatalf("a document that does not exist is a %q", missing[0].Outcome)
	}
	if missing[0].Rule != "" || missing[0].Documents[0].Source != "" {
		t.Errorf("the record says more about a document nobody may see than a refusal does: %+v", missing[0])
	}
}

// TestTheRuleThatAdmittedSomebodyIsOnTheRecord, because "they own it" and "it is
// public to the tenant" are different facts about an access and an investigation
// has to be able to tell them apart. The reference that matched is not, because
// it is a group name.
func TestTheRuleThatAdmittedSomebodyIsOnTheRecord(t *testing.T) {
	sink, log, h := trailed(t)

	got := written(t, sink, log, h, "/api/v1/documents/d1")
	if len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
	if got[0].Rule != string(acl.RuleListed) {
		t.Errorf("the record says the rule was %q, want %q", got[0].Rule, acl.RuleListed)
	}
	if got[0].Documents[0].Source != "gdrive" {
		t.Errorf("the record does not say where the document came from: %+v", got[0].Documents)
	}
}

// TestHowMuchContentLeftIsOnTheRecord. The question after an incident is not
// only which documents somebody opened but how much of the corpus they took,
// and a response size does not answer that.
func TestHowMuchContentLeftIsOnTheRecord(t *testing.T) {
	sink, log, h := trailed(t)

	got := written(t, sink, log, h, "/api/v1/documents/img/content")
	if len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
	if got[0].Action != audit.Content || got[0].Outcome != audit.Served {
		t.Fatalf("the record is a %q %q", got[0].Outcome, got[0].Action)
	}
	if want := int64(len(square(t))); got[0].Bytes != want {
		t.Errorf("the record says %d bytes left, want %d", got[0].Bytes, want)
	}
}

// TestASearchRecordsThePageAndTheSizeOfWhatMatched, which are two different
// numbers: what somebody saw, and how much there was to see.
func TestASearchRecordsThePageAndTheSizeOfWhatMatched(t *testing.T) {
	sink, log, h := trailed(t)

	got := written(t, sink, log, h, "/api/v1/search?q=payments&limit=1")
	if len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
	rec := got[0]
	if rec.Action != audit.Search || rec.Query != "payments" {
		t.Errorf("the record is a %q for %q", rec.Action, rec.Query)
	}
	if len(rec.Documents) != 1 || rec.Documents[0].ID != "d1" {
		t.Errorf("the page on the record is %v", rec.Documents)
	}
	if rec.Count < 1 {
		t.Errorf("the record says %d documents matched", rec.Count)
	}
}

// TestSomebodyElsesResultsAreNotOnSomebodyElsesTrail. The search is filtered by
// the driver and the record is built from what came back, so a document the
// caller may not read cannot appear on their record. This is the test that
// would fail if the record were ever built from the query rather than from the
// answer.
func TestSomebodyElsesResultsAreNotOnSomebodyElsesTrail(t *testing.T) {
	sink, log, h := trailed(t)

	got := written(t, sink, log, h, "/api/v1/search?q=numbers")
	if len(got) != 1 {
		t.Fatalf("%d records, want 1", len(got))
	}
	for _, item := range got[0].Documents {
		if item.ID == "secret" {
			t.Fatalf("a document this caller may not read is on their record: %v", got[0].Documents)
		}
	}
}

// square is a real PNG, small enough to be cheap and large enough to be one.
func square(t *testing.T) []byte {
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
