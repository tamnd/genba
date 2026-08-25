package recheck_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/recheck"
)

// source is a checker that answers from a set and remembers what it was asked.
type source struct {
	mu      sync.Mutex
	allowed map[string]bool
	calls   [][]string
	err     error
	block   time.Duration
	omit    string
}

func (s *source) Allowed(ctx context.Context, _ *acl.Principal, ids []string) (map[string]bool, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), ids...))
	block, err := s.block, s.err
	s.mu.Unlock()

	if block > 0 {
		select {
		case <-time.After(block):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == s.omit {
			continue
		}
		out[id] = s.allowed[id]
	}
	return out, nil
}

func (s *source) asked() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.calls...)
}

func mei() *acl.Principal {
	return &acl.Principal{Tenant: "acme", Subject: "u_mei", Kind: acl.KindUser}
}

func items(pairs ...string) []recheck.Item {
	out := make([]recheck.Item, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, recheck.Item{Source: pairs[i], ID: pairs[i+1]})
	}
	return out
}

// TestASourceNobodyRegisteredIsLeftAlone, which is what off by default means in
// practice. A deployment that has not said its source can answer this quickly
// does not have its search latency decided by somebody else's API.
func TestASourceNobodyRegisteredIsLeftAlone(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": false}}
	s := recheck.New()
	s.Add("gdrive", drive)

	got := s.Allowed(t.Context(), mei(), items("slack", "c1", "slack", "c2"))
	if !got["c1"] || !got["c2"] {
		t.Errorf("an unchecked source was filtered: %v", got)
	}
	if calls := drive.asked(); len(calls) != 0 {
		t.Errorf("the drive checker was asked about somebody else's documents: %v", calls)
	}
}

// TestARevokedDocumentIsRemoved is the feature.
func TestARevokedDocumentIsRemoved(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": true, "d2": false}}
	s := recheck.New()
	s.Add("gdrive", drive)

	got := s.Allowed(t.Context(), mei(), items("gdrive", "d1", "gdrive", "d2"))
	if !got["d1"] {
		t.Error("a document nobody revoked was removed")
	}
	if got["d2"] {
		t.Error("a revoked document is still in the results")
	}
	if st := s.Stats()["gdrive"]; st.Checked != 2 || st.Denied != 1 || st.Failed != 0 {
		t.Errorf("stats = %+v", st)
	}
}

// TestOnePageIsOneQuestion. The whole reason this can run on the request path
// is that a page of ten documents costs one round trip and not ten.
func TestOnePageIsOneQuestion(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": true, "d2": true, "d3": true}}
	s := recheck.New()
	s.Add("gdrive", drive)

	s.Allowed(t.Context(), mei(), items("gdrive", "d1", "gdrive", "d2", "gdrive", "d3"))
	calls := drive.asked()
	if len(calls) != 1 {
		t.Fatalf("%d calls for one page, want 1: %v", len(calls), calls)
	}
	if len(calls[0]) != 3 {
		t.Errorf("the source was asked about %v", calls[0])
	}
}

// TestTheSameDocumentTwiceIsAskedOnce, because a page that cites a document in
// a result and again in a quote is one document as far as the source is
// concerned.
func TestTheSameDocumentTwiceIsAskedOnce(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": true}}
	s := recheck.New()
	s.Add("gdrive", drive)

	s.Allowed(t.Context(), mei(), items("gdrive", "d1", "gdrive", "d1"))
	if calls := drive.asked(); len(calls) != 1 || len(calls[0]) != 1 {
		t.Errorf("the source was asked %v", calls)
	}
}

// TestTheSecondPageAsksNothing, which is what makes paging through results
// affordable rather than quadratic in round trips.
func TestTheSecondPageAsksNothing(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": true}}
	s := recheck.New()
	s.Add("gdrive", drive)

	for range 3 {
		if got := s.Allowed(t.Context(), mei(), items("gdrive", "d1")); !got["d1"] {
			t.Fatal("a document that was allowed came back denied")
		}
	}
	if calls := drive.asked(); len(calls) != 1 {
		t.Errorf("%d calls for three pages of the same document", len(calls))
	}
	if hits := s.CacheStats().Hits; hits != 2 {
		t.Errorf("the answer cache reports %d hits", hits)
	}
}

// TestAnAnswerIsAboutOnePerson. Two people asking about the same document is
// two questions, because the answer is a fact about them and not about it.
func TestAnAnswerIsAboutOnePerson(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": true}}
	s := recheck.New()
	s.Add("gdrive", drive)

	s.Allowed(t.Context(), mei(), items("gdrive", "d1"))
	other := &acl.Principal{Tenant: "acme", Subject: "u_raj", Kind: acl.KindUser}
	s.Allowed(t.Context(), other, items("gdrive", "d1"))

	if calls := drive.asked(); len(calls) != 2 {
		t.Errorf("%d questions for two people, want 2", len(calls))
	}
}

// TestACheckThatFailsRemovesTheRow is the decision the package is built around.
//
// A source that cannot answer
// leaves us holding a document whose permissions we last read at the sync, and
// the honest thing to do with it is not show it. The cost is a document
// somebody is entitled to going missing for a moment, which they can report.
// The other way round is a document going out that should not have, which
// nobody reports and nobody can take back.
func TestACheckThatFailsRemovesTheRow(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": true}, err: errors.New("the drive is unhappy")}
	s := recheck.New()
	s.Add("gdrive", drive)

	if got := s.Allowed(t.Context(), mei(), items("gdrive", "d1")); got["d1"] {
		t.Error("a document whose check failed was shown anyway")
	}
	if st := s.Stats()["gdrive"]; st.Failed != 1 || st.Denied != 0 {
		t.Errorf("a failure was counted as %+v, and a failure is not a refusal", st)
	}
}

// TestAFailureIsNotRemembered, because a source that could not answer once will
// probably answer in a second, and caching the failure would turn a moment of
// trouble into ten seconds of a document nobody can find.
func TestAFailureIsNotRemembered(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": true}, err: errors.New("not now")}
	s := recheck.New()
	s.Add("gdrive", drive)

	s.Allowed(t.Context(), mei(), items("gdrive", "d1"))

	drive.mu.Lock()
	drive.err = nil
	drive.mu.Unlock()

	if got := s.Allowed(t.Context(), mei(), items("gdrive", "d1")); !got["d1"] {
		t.Error("the source recovered and the document is still missing")
	}
}

// TestAnIdTheSourceLeftOutIsNotAnAllow. An implementation that cannot answer
// about one document says so by leaving it out, and does not have to decide on
// its own what the safe answer is.
func TestAnIdTheSourceLeftOutIsNotAnAllow(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": true, "d2": true}, omit: "d2"}
	s := recheck.New()
	s.Add("gdrive", drive)

	got := s.Allowed(t.Context(), mei(), items("gdrive", "d1", "gdrive", "d2"))
	if !got["d1"] {
		t.Error("the document the source answered about was removed")
	}
	if got["d2"] {
		t.Error("a document the source said nothing about was shown")
	}
	if st := s.Stats()["gdrive"]; st.Failed != 1 {
		t.Errorf("a silent id was counted as %+v", st)
	}
}

// TestACheckThatMissesItsDeadlineRemovesTheRow, and does not hold the response
// open while it thinks about it.
func TestACheckThatMissesItsDeadlineRemovesTheRow(t *testing.T) {
	drive := &source{allowed: map[string]bool{"d1": true}, block: time.Minute}
	s := recheck.New(recheck.WithTimeout(20 * time.Millisecond))
	s.Add("gdrive", drive)

	start := time.Now()
	got := s.Allowed(t.Context(), mei(), items("gdrive", "d1"))
	took := time.Since(start)

	if got["d1"] {
		t.Error("a document whose check never came back was shown")
	}
	if took > 5*time.Second {
		t.Errorf("the check took %v, and the timeout is not binding anything", took)
	}
	if st := s.Stats()["gdrive"]; st.Failed != 1 {
		t.Errorf("a timeout was counted as %+v", st)
	}
}

// TestASlowSourceDoesNotTakeAFastOneWithIt puts a page that spans two sources
// to both at once, and keeps the one that answered inside the budget.
//
// The one that answered in time is still on the screen. The alternative is one source
// having a bad afternoon emptying every page of results in the company.
func TestASlowSourceDoesNotTakeAFastOneWithIt(t *testing.T) {
	slow := &source{allowed: map[string]bool{"d1": true}, block: time.Minute}
	fast := &source{allowed: map[string]bool{"c1": true}}
	s := recheck.New(recheck.WithTimeout(50 * time.Millisecond))
	s.Add("gdrive", slow)
	s.Add("slack", fast)

	got := s.Allowed(t.Context(), mei(), items("gdrive", "d1", "slack", "c1"))
	if !got["c1"] {
		t.Error("the source that answered in time lost its results to the one that did not")
	}
	if got["d1"] {
		t.Error("the source that did not answer had its results shown")
	}
}

// TestNothingIsAskedWhenThereIsNothingToAsk, which is the shape of every
// request on a deployment that has not turned this on.
func TestNothingIsAskedWhenThereIsNothingToAsk(t *testing.T) {
	s := recheck.New()
	if got := s.Allowed(t.Context(), mei(), items("slack", "c1")); !got["c1"] {
		t.Error("a set with no checkers in it removed a result")
	}
	if got := s.Allowed(t.Context(), mei(), nil); len(got) != 0 {
		t.Errorf("an empty page produced %v", got)
	}
	if len(s.Sources()) != 0 {
		t.Errorf("a set with no checkers reports %v", s.Sources())
	}
}

// TestASourceCanBeCheckedFromOneAfternoonOnward, because a connector configured
// from the administration screen arrives while the server is serving.
func TestASourceCanBeCheckedFromOneAfternoonOnward(t *testing.T) {
	s := recheck.New()
	if got := s.Allowed(t.Context(), mei(), items("gdrive", "d1")); !got["d1"] {
		t.Fatal("an unchecked source was filtered")
	}

	s.Add("gdrive", recheck.Func(func(context.Context, *acl.Principal, []string) (map[string]bool, error) {
		return map[string]bool{"d1": false}, nil
	}))
	if got := s.Allowed(t.Context(), mei(), items("gdrive", "d1")); got["d1"] {
		t.Error("the source gained a checker and the document was not checked")
	}

	s.Add("gdrive", nil)
	if got := s.Allowed(t.Context(), mei(), items("gdrive", "d2")); !got["d2"] {
		t.Error("the checker was removed and the source is still being filtered")
	}
}

// TestEveryPageIsCheckedUnderLoad, which is the property a lock in the wrong
// place would cost quietly.
func TestEveryPageIsCheckedUnderLoad(t *testing.T) {
	var asked atomic.Int64
	// The deadline is long because this test is about the lock and not about the
	// clock. Four hundred checks running at once under the race detector will
	// miss a twenty millisecond budget on a busy machine, and a check that timed
	// out is counted as a failure, which would make this fail for the one reason
	// it is not asking about.
	s := recheck.New(recheck.WithCache(0, cache.Off), recheck.WithTimeout(time.Minute))
	s.Add("gdrive", recheck.Func(func(_ context.Context, _ *acl.Principal, ids []string) (map[string]bool, error) {
		asked.Add(int64(len(ids)))
		out := make(map[string]bool, len(ids))
		for _, id := range ids {
			out[id] = id != "gone"
		}
		return out, nil
	}))

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 50 {
				page := items("gdrive", "gone", "gdrive", string(rune('a'+w))+string(rune('0'+i%10)))
				got := s.Allowed(t.Context(), mei(), page)
				if got["gone"] {
					t.Errorf("the revoked document was shown")
					return
				}
			}
		})
	}
	wg.Wait()

	if got := asked.Load(); got != 800 {
		t.Errorf("%d documents were checked, want 800", got)
	}
	if st := s.Stats()["gdrive"]; st.Checked != 800 || st.Denied != 400 {
		t.Errorf("stats = %+v", st)
	}
}
