package storetest

import (
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/store"
)

// A report is the one write in this interface that anybody who can read a
// document may make, which is exactly why the permission cases here matter. The
// rule that does not move is the one every capability shares: a report is
// written and read through the principal, so a reader who cannot see the
// document cannot report it and cannot learn that anybody else did.
//
// The rest of these cases are about the two questions a driver has to answer
// from the same rows. What has been said about this document, which is a page
// of ids and draws a mark, and what has been said about mine, which is an inbox
// and is the half that makes reporting worth doing. A driver that answers the
// first and not the second has built a flag nobody reads.

// reporter skips a case for a driver that cannot remember a report.
func reporter(t *testing.T, s store.Store) store.Reporter {
	t.Helper()
	r, ok := s.(store.Reporter)
	if !ok {
		t.Skip("driver does not implement store.Reporter")
	}
	return r
}

// colleague is a second reader in the same group, so that a case about two
// people reporting the same document has two people to report it with.
func colleague() *acl.Principal {
	return &acl.Principal{
		Tenant:     "acme",
		Subject:    "u_sam",
		Identities: []acl.Identity{{Source: "gdrive", Value: "sam@acme.com"}},
		Groups:     acl.GroupSet{Version: 1, Members: []string{"gdrive:eng@acme.com"}},
	}
}

var (
	mei = doc.Person{Subject: "u_mei", Name: "Mei Tanaka", Email: "mei@acme.com"}
	sam = doc.Person{Subject: "u_sam", Name: "Sam Okafor", Email: "sam@acme.com"}
)

// complaint is what the reader says about a document, at a known minute.
func complaint(id string, minute int) store.Report {
	return store.Report{
		Doc:  id,
		By:   mei,
		At:   at(minute),
		Note: "the failover step names a cluster we turned off in March",
	}
}

// hers is a document the reader owns, which is what puts it in their inbox.
func hers(id string) doc.Document {
	d := document(id, readable())
	d.Owner = mei
	return d
}

func said(t *testing.T, r store.Reporter, p *acl.Principal, ids ...string) map[string]store.Staleness {
	t.Helper()
	got, err := r.Reports(t.Context(), p, ids)
	if err != nil {
		t.Fatalf("Reports: %v", err)
	}
	return got
}

func inbox(t *testing.T, r store.Reporter, p *acl.Principal, limit int) []store.Flagged {
	t.Helper()
	got, err := r.Reported(t.Context(), p, limit)
	if err != nil {
		t.Fatalf("Reported: %v", err)
	}
	return got
}

func report(t *testing.T, r store.Reporter, p *acl.Principal, one store.Report) {
	t.Helper()
	if err := r.Report(t.Context(), p, one); err != nil {
		t.Fatalf("Report: %v", err)
	}
}

func testReportRoundTrip(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))

	want := complaint("d1", 0)
	report(t, r, reader(), want)

	got, ok := said(t, r, reader(), "d1")["d1"]
	if !ok {
		t.Fatalf("the document was reported and came back with nothing said about it")
	}
	if got.Count != 1 {
		t.Errorf("one person reported it and the count is %d", got.Count)
	}
	// The name and the sentence are the whole point. A count with neither is the
	// anonymous flag this type exists instead of.
	if got.Last.By != want.By {
		t.Errorf("the report is against %+v and was made by %+v", got.Last.By, want.By)
	}
	if got.Last.Note != want.Note || !got.Last.At.Equal(want.At) {
		t.Errorf("what was said came back as %q at %v", got.Last.Note, got.Last.At)
	}
}

// Somebody reporting the same document twice found it stale again. That is one
// person, not two, and a count that says otherwise is a count an owner learns
// to ignore.
func testReportReplacesTheirOwn(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))

	second := complaint("d1", 5)
	second.Note = "still wrong, and now the diagram is wrong too"
	report(t, r, reader(), complaint("d1", 0))
	report(t, r, reader(), second)

	got := said(t, r, reader(), "d1")["d1"]
	if got.Count != 1 {
		t.Errorf("the same person reported it twice and the count is %d", got.Count)
	}
	if got.Last.Note != second.Note {
		t.Errorf("the standing report says %q rather than what they said last", got.Last.Note)
	}
}

// Two people are two, and the mark says the most recent thing said.
func testReportCounts(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))

	later := complaint("d1", 10)
	later.By = sam
	later.Note = "the addresses in the appendix are the old ones"
	report(t, r, reader(), complaint("d1", 0))
	report(t, r, colleague(), later)

	got := said(t, r, reader(), "d1")["d1"]
	if got.Count != 2 {
		t.Errorf("two people reported it and the count is %d", got.Count)
	}
	if got.Last.Note != later.Note {
		t.Errorf("the mark says %q rather than the most recent thing said", got.Last.Note)
	}
}

// A reader who cannot see the document cannot report it and cannot learn that
// anybody else did. Both halves, because either one on its own answers whether
// an id is real.
func testReportPermissions(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))

	unseen := complaint("d1", 0)
	unseen.By = doc.Person{Subject: "u_kenji", Name: "Kenji Sato", Email: "kenji@acme.com"}
	if err := r.Report(t.Context(), stranger(), unseen); err != nil {
		t.Fatalf("reporting something you cannot see is a write that does not happen: %v", err)
	}
	if got := said(t, r, reader(), "d1"); len(got) != 0 {
		t.Errorf("a reader who cannot see the document reported it anyway: %+v", got)
	}

	report(t, r, reader(), complaint("d1", 0))
	if got := said(t, r, stranger(), "d1"); len(got) != 0 {
		t.Errorf("a reader who cannot see the document was told it had been reported: %+v", got)
	}
	if got := inbox(t, r, stranger(), 10); len(got) != 0 {
		t.Errorf("the inbox handed out a document its reader cannot see: %+v", got)
	}
}

// The crawl comes round every night. A report it erased would be a report
// nobody ever acts on, because the source that made the document stale is the
// thing that rewrites it.
func testReportSurvivesRewrite(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))
	report(t, r, reader(), complaint("d1", 0))

	again := hers("d1")
	again.Title = "runbook d1, revised"
	mustPut(t, s, again)

	if got := said(t, r, reader(), "d1"); len(got) != 1 {
		t.Errorf("a crawl erased what somebody said about the document: %+v", got)
	}
}

// Clearing is whoever is accountable saying they have dealt with it, and it
// clears what everybody said rather than one person's line, because the work
// was the document.
func testResolve(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"), hers("d2"))
	report(t, r, reader(), complaint("d1", 0))
	report(t, r, reader(), complaint("d2", 0))

	if err := r.Resolve(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got := said(t, r, reader(), "d1", "d2")
	if _, ok := got["d1"]; ok {
		t.Errorf("the report was resolved and is still standing")
	}
	if _, ok := got["d2"]; !ok {
		t.Errorf("resolving one document cleared another")
	}

	// Again, because the API calls this after a verification without asking
	// first, and a second call has to be as quiet as the first.
	if err := r.Resolve(t.Context(), reader(), "d1"); err != nil {
		t.Errorf("resolving a document nobody reported: %v", err)
	}
}

// Withdrawing is taking back your own sentence, which is a different thing from
// clearing what everybody said. The reporter here is a reader with no permission
// on the document, so the row their key wrote is the only row they can touch: a
// withdraw that took the document with it would be handing every reader the
// owner's button under another name.
func testWithdraw(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))

	theirs := complaint("d1", 10)
	theirs.By = sam
	theirs.Note = "the addresses in the appendix are the old ones"
	report(t, r, reader(), complaint("d1", 0))
	report(t, r, colleague(), theirs)

	if err := r.Withdraw(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	got, ok := said(t, r, reader(), "d1")["d1"]
	if !ok {
		t.Fatalf("one of two reports was withdrawn and now the document has nothing said about it")
	}
	if got.Count != 1 || got.Last.Note != theirs.Note {
		t.Errorf("what is left is %d reports saying %q", got.Count, got.Last.Note)
	}
	if got.Mine {
		t.Errorf("the reader took their report back and is still counted as one of the people who made one")
	}

	// Again, because a second click on a button that has already done its work is
	// something that happens on a slow connection.
	if err := r.Withdraw(t.Context(), reader(), "d1"); err != nil {
		t.Errorf("withdrawing a report that is no longer there: %v", err)
	}
}

// The last one out leaves the document unreported, the same way a resolve does,
// because a mark with nothing behind it is a warning nobody can act on and an
// inbox row with nothing behind it is worse.
func testWithdrawLast(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))
	report(t, r, reader(), complaint("d1", 0))

	if err := r.Withdraw(t.Context(), reader(), "d1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if got := said(t, r, reader(), "d1"); len(got) != 0 {
		t.Errorf("the only report was withdrawn and the document is still marked: %+v", got)
	}
	if got := inbox(t, r, reader(), 10); len(got) != 0 {
		t.Errorf("the withdrawn report is still in front of the person who owns the document: %+v", got)
	}
	if err := r.Withdraw(t.Context(), reader(), "missing"); err != nil {
		t.Errorf("withdrawing from a document that is not there: %v", err)
	}
}

// The visibility predicate does not move for this call either. It cannot matter
// today, because a key only ever matches the row it wrote, but a driver that
// left it out would answer a question about which documents exist the first time
// somebody counted the rows it deleted.
func testWithdrawPermissions(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))
	report(t, r, reader(), complaint("d1", 0))

	if err := r.Withdraw(t.Context(), stranger(), "d1"); err != nil {
		t.Fatalf("withdrawing from something you cannot see is a write that does not happen: %v", err)
	}
	if got := said(t, r, reader(), "d1"); len(got) != 1 {
		t.Errorf("a reader who cannot see the document withdrew a report on it: %+v", got)
	}
}

// Whether the person asking is one of the people who complained, which is the
// question an interface has to answer before it can offer to take a report back.
//
// It is asked here with somebody else complaining most recently, because that is
// the case a caller comparing the name on the standing report against their own
// gets wrong, and it is the usual case: the mark carries the last thing said and
// the reader asking is rarely the last person to have said it.
func testReportMine(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"), hers("d2"))

	later := complaint("d1", 10)
	later.By = sam
	report(t, r, reader(), complaint("d1", 0))
	report(t, r, colleague(), later)
	report(t, r, colleague(), complaint("d2", 15))

	got := said(t, r, reader(), "d1", "d2")
	if !got["d1"].Mine {
		t.Errorf("the reader reported it and what came back says somebody else did")
	}
	if got["d2"].Mine {
		t.Errorf("a document the reader never reported came back as theirs")
	}
	if !said(t, r, colleague(), "d1")["d1"].Mine {
		t.Errorf("the colleague reported it and what came back says somebody else did")
	}

	// The inbox answers it too, because the owner of a document is allowed to be
	// one of the people who reported it and the panel offers the same way out.
	for _, f := range inbox(t, r, reader(), 10) {
		if want := f.Document.ID == "d1"; f.Stale.Mine != want {
			t.Errorf("the panel says the owner reported %s is %v", f.Document.ID, f.Stale.Mine)
		}
	}

	if err := r.Withdraw(t.Context(), colleague(), "d1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	if said(t, r, colleague(), "d1")["d1"].Mine {
		t.Errorf("the colleague took their report back and is still counted as one of the people who made one")
	}
	if !said(t, r, reader(), "d1")["d1"].Mine {
		t.Errorf("one person withdrawing took somebody else's report with it")
	}
}

func testReportDelete(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))
	report(t, r, reader(), complaint("d1", 0))
	if err := s.Delete(t.Context(), "d1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	mustPut(t, s, hers("d1"))
	if got := said(t, r, reader(), "d1"); len(got) != 0 {
		t.Errorf("a document written under the id of a deleted one inherited its reports: %+v", got)
	}
}

// The inbox. This is the half that makes reporting worth doing: what has been
// said about mine, rather than what has been said about this.
func testReportedIsMine(t *testing.T, s store.Store) {
	r := reporter(t, s)
	theirs := document("d2", readable())
	theirs.Owner = sam
	theirs.Author = sam
	mustPut(t, s, hers("d1"), theirs)
	report(t, r, colleague(), complaint("d1", 0))
	report(t, r, colleague(), complaint("d2", 5))

	got := inbox(t, r, reader(), 10)
	if len(got) != 1 || got[0].Document.ID != "d1" {
		t.Fatalf("the inbox holds %d entries and the first is not the reader's own document: %+v", len(got), got)
	}
	if got[0].Stale.Count != 1 || got[0].Stale.Last.Note == "" {
		t.Errorf("the inbox entry says nothing that can be acted on: %+v", got[0].Stale)
	}
	// The document comes with it, because a list of ids is a list somebody then
	// resolves one at a time.
	if got[0].Document.Title == "" {
		t.Errorf("the inbox entry carries no document")
	}
}

// Being the author is enough. A document a connector attributed to the account
// that ran the import is exactly the case where nobody would ever see the
// report.
func testReportedByAuthorship(t *testing.T, s store.Store) {
	r := reporter(t, s)
	d := document("d1", readable())
	d.Owner = derived
	d.Author = mei
	mustPut(t, s, d)
	report(t, r, colleague(), complaint("d1", 0))

	if got := inbox(t, r, reader(), 10); len(got) != 1 {
		t.Errorf("the person who wrote the document was not told it is out of date: %+v", got)
	}
}

// Newest first, and a limit that means what it says. An inbox in an arbitrary
// order is a list somebody reads the top of and then reads again tomorrow.
func testReportedOrder(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"), hers("d2"), hers("d3"))
	for i, id := range []string{"d2", "d1", "d3"} {
		report(t, r, colleague(), complaint(id, i*5))
	}

	got := inbox(t, r, reader(), 2)
	if len(got) != 2 {
		t.Fatalf("a limit of two returned %d entries", len(got))
	}
	if got[0].Document.ID != "d3" || got[1].Document.ID != "d1" {
		t.Errorf("the inbox is not most recently reported first: %s then %s",
			got[0].Document.ID, got[1].Document.ID)
	}
	if none := inbox(t, r, reader(), 0); len(none) != 0 {
		t.Errorf("a limit of zero returned %d entries", len(none))
	}
}

// A report with nobody's name on it is the anonymous flag this is instead of,
// and it is refused rather than stored as a number.
func testReportRejectsAnonymous(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"))

	if err := r.Report(t.Context(), reader(), store.Report{Doc: "d1", At: at(0)}); err == nil {
		t.Errorf("a report by nobody was accepted")
	}
}

// A page of ids is one question, because the caller is drawing a screen.
func testReportBatch(t *testing.T, s store.Store) {
	r := reporter(t, s)
	mustPut(t, s, hers("d1"), hers("d2"), hers("d3"))
	report(t, r, reader(), complaint("d1", 0))
	report(t, r, reader(), complaint("d3", 0))

	got := said(t, r, reader(), "d1", "d2", "d3", "missing")
	if len(got) != 2 {
		t.Fatalf("three documents and a missing one answered with %d entries: %+v", len(got), got)
	}
	for _, id := range []string{"d1", "d3"} {
		if got[id].Doc != id {
			t.Errorf("the entry for %s says it is about %q", id, got[id].Doc)
		}
	}
	if _, ok := got["d2"]; ok {
		t.Errorf("a document nobody reported came back with a zero entry")
	}
}
