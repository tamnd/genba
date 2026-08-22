package storetest

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/tamnd/genba/store"
)

// RunFeeds checks a driver's [store.Feeds] against what an operator configuring
// a connector from the interface relies on.
//
// The cases here are all versions of one worry. This is the only thing in the
// store that is not a document, so it is the only thing a driver can get wrong
// without a search noticing, and the way somebody finds out is that a connector
// they configured last month is not running after a restart.
func RunFeeds(t *testing.T, newStore Factory) {
	t.Helper()
	for _, c := range feedCases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })

			f, ok := s.(store.Feeds)
			if !ok {
				t.Skip("this driver does not implement store.Feeds")
			}
			c.run(t, s, f)
		})
	}
}

type feedCase struct {
	name string
	run  func(t *testing.T, s store.Store, f store.Feeds)
}

var feedCases = []feedCase{
	{"a feed comes back the way it went in", testFeedRoundTrip},
	{"saving again replaces it and keeps its age", testFeedReplace},
	{"another tenant's feeds are somebody else's", testFeedTenant},
	{"dropping one leaves the rest alone", testFeedDrop},
	{"dropping one that was never there is not an error", testFeedDropMissing},
	{"dropping a feed keeps the documents it indexed", testFeedDropKeepsDocuments},
	{"a feed with no source is refused", testFeedNeedsSource},
}

// feed is a saved feed read back by source, and fails the test if it is gone.
func feed(t *testing.T, f store.Feeds, tenant, source string) store.Feed {
	t.Helper()
	list, err := f.Feeds(t.Context(), tenant)
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	i := slices.IndexFunc(list, func(x store.Feed) bool { return x.Source == source })
	if i < 0 {
		t.Fatalf("feed %q is not in %v", source, sources(list))
	}
	return list[i]
}

func sources(list []store.Feed) []string {
	out := make([]string, 0, len(list))
	for _, f := range list {
		out = append(out, f.Source)
	}
	slices.Sort(out)
	return out
}

func save(t *testing.T, f store.Feeds, in store.Feed) {
	t.Helper()
	if err := f.SaveFeed(t.Context(), in); err != nil {
		t.Fatalf("SaveFeed(%q): %v", in.Source, err)
	}
}

func testFeedRoundTrip(t *testing.T, _ store.Store, f store.Feeds) {
	in := store.Feed{
		Tenant:  "acme",
		Source:  "handbook",
		Kind:    "files",
		Enabled: true,
		Config:  json.RawMessage(`{"dir":"/srv/handbook","acl":"owners","watch":true}`),
		By:      "u_ops",
	}
	save(t, f, in)

	got := feed(t, f, "acme", "handbook")
	if got.Kind != in.Kind || got.Enabled != in.Enabled || got.By != in.By {
		t.Errorf("saved %+v, read back %+v", in, got)
	}
	// Byte for byte, not decoded and compared. The store is told this is opaque
	// and a driver that reformatted it would be a driver that decides what a
	// connector's settings mean.
	if string(got.Config) != string(in.Config) {
		t.Errorf("config came back as %s, want %s", got.Config, in.Config)
	}
	if got.Created.IsZero() || got.Updated.IsZero() {
		t.Errorf("the driver did not stamp the row: created %v, updated %v", got.Created, got.Updated)
	}
	if got.Updated.Before(got.Created) {
		t.Errorf("updated %v is before created %v", got.Updated, got.Created)
	}
}

func testFeedReplace(t *testing.T, _ store.Store, f store.Feeds) {
	in := store.Feed{Tenant: "acme", Source: "handbook", Kind: "files", Enabled: true, By: "u_ops"}
	save(t, f, in)
	first := feed(t, f, "acme", "handbook")

	in.Enabled = false
	in.By = "u_kenji"
	in.Config = json.RawMessage(`{"dir":"/srv/handbook"}`)
	save(t, f, in)

	got := feed(t, f, "acme", "handbook")
	if got.Enabled {
		t.Error("the feed was switched off and came back switched on")
	}
	if got.By != "u_kenji" {
		t.Errorf("the row says %q wrote it, want u_kenji", got.By)
	}
	// The age of the row does not move when it is edited. An operator reading a
	// connector that has been failing since it was set up needs to be able to
	// tell that from one somebody added this morning, and if editing reset this
	// then every connector would look new.
	if !got.Created.Equal(first.Created) {
		t.Errorf("created moved from %v to %v when the feed was edited", first.Created, got.Created)
	}
	if got.Updated.Before(first.Updated) {
		t.Errorf("updated went backwards from %v to %v", first.Updated, got.Updated)
	}
	if list, err := f.Feeds(t.Context(), "acme"); err != nil {
		t.Fatalf("Feeds: %v", err)
	} else if len(list) != 1 {
		t.Errorf("saving the same source twice left %v, want one row", sources(list))
	}
}

func testFeedTenant(t *testing.T, _ store.Store, f store.Feeds) {
	save(t, f, store.Feed{Tenant: "acme", Source: "handbook", Kind: "files"})
	save(t, f, store.Feed{Tenant: "other", Source: "handbook", Kind: "files"})

	// The same source name under two tenants is two connectors, not one. It is
	// the case a driver keyed on the source alone passes every other test and
	// fails here, by handing one company's configuration to another.
	acme, err := f.Feeds(t.Context(), "acme")
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(acme) != 1 || acme[0].Tenant != "acme" {
		t.Fatalf("acme sees %+v, want its own one row", acme)
	}
	other, err := f.Feeds(t.Context(), "other")
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if len(other) != 1 || other[0].Tenant != "other" {
		t.Fatalf("other sees %+v, want its own one row", other)
	}
}

func testFeedDrop(t *testing.T, _ store.Store, f store.Feeds) {
	save(t, f, store.Feed{Tenant: "acme", Source: "handbook", Kind: "files"})
	save(t, f, store.Feed{Tenant: "acme", Source: "archive", Kind: "bucket"})
	save(t, f, store.Feed{Tenant: "other", Source: "handbook", Kind: "files"})

	if err := f.DropFeed(t.Context(), "acme", "handbook"); err != nil {
		t.Fatalf("DropFeed: %v", err)
	}
	list, err := f.Feeds(t.Context(), "acme")
	if err != nil {
		t.Fatalf("Feeds: %v", err)
	}
	if want := []string{"archive"}; !slices.Equal(sources(list), want) {
		t.Errorf("acme has %v, want %v", sources(list), want)
	}
	// Dropped for one tenant and not for the other, which is the same key
	// mistake as above seen from the delete side.
	if list, err := f.Feeds(t.Context(), "other"); err != nil {
		t.Fatalf("Feeds: %v", err)
	} else if len(list) != 1 {
		t.Errorf("dropping acme's handbook left other with %v", sources(list))
	}
}

func testFeedDropMissing(t *testing.T, _ store.Store, f store.Feeds) {
	// Removing a connector twice is the same as removing it once, so that a
	// retry after a timeout is not an error somebody has to read.
	if err := f.DropFeed(t.Context(), "acme", "never-configured"); err != nil {
		t.Errorf("dropping a feed that was never there: %v", err)
	}
}

func testFeedDropKeepsDocuments(t *testing.T, s store.Store, f store.Feeds) {
	// The document and the feed carry the same source, which is the whole point
	// of the case: these two rows are joinable and a driver that joined them on
	// delete would pass every other test here.
	if err := s.Put(t.Context(), document("gdrive:1", readable())); err != nil {
		t.Fatalf("Put: %v", err)
	}
	save(t, f, store.Feed{Tenant: "acme", Source: "gdrive", Kind: "files"})

	if err := f.DropFeed(t.Context(), "acme", "gdrive"); err != nil {
		t.Fatalf("DropFeed: %v", err)
	}
	// Forgetting how a corpus was read is not a decision to delete the corpus.
	// A driver that cascaded here would make an operator's undo cost a full
	// crawl, and on a large source that is hours.
	if _, err := s.Get(t.Context(), reader(), "gdrive:1"); err != nil {
		t.Errorf("dropping the feed took its documents with it: %v", err)
	}
}

func testFeedNeedsSource(t *testing.T, _ store.Store, f store.Feeds) {
	if err := f.SaveFeed(t.Context(), store.Feed{Tenant: "acme", Kind: "files"}); err == nil {
		t.Error("a feed with no source was accepted, so it can be written and never addressed again")
	}
}
