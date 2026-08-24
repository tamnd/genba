package fssource_test

import (
	"bytes"
	"context"
	"errors"
	"image"
	pngenc "image/png"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/fssource"
	"github.com/tamnd/genba/doc"
)

// tree writes a directory tree from a map of relative path to contents.
//
// The files are dated a minute ago. A corpus that was written in the same
// millisecond as the sync reading it is not a corpus, and it is not what these
// tests are about either: a sync will not claim to have covered a moment it is
// still inside of, so a tree written now leaves a cursor that deliberately sits
// behind it and the file is read once more on the next sync. Dating the fixture
// is how the tests ask their own question instead of that one.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	ago := time.Now().Add(-time.Minute)
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, ago, ago); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// collect runs one sync and returns what it emitted.
func collect(t *testing.T, s *fssource.Source, from connector.Cursor) ([]doc.Document, connector.Cursor) {
	t.Helper()
	var got []doc.Document
	next, err := s.Sync(t.Context(), from, func(_ context.Context, ch connector.Change) error {
		got = append(got, ch.Document)
		return nil
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].ID < got[j].ID })
	return got, next
}

func ids(docs []doc.Document) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.ID)
	}
	return out
}

func TestItReadsATreeIntoDocuments(t *testing.T) {
	root := tree(t, map[string]string{
		"README.md":            "# The project\n\nWhat it does.\n",
		"docs/install.md":      "# Installing\n\nRun the thing.\n",
		"docs/deep/nested.md":  "no heading here, just a line\n",
		"cmd/main.go":          "package main\n",
		"assets/logo.png":      "\x89PNG\r\n\x1a\n binary",
		".hidden/secret.md":    "# should not appear\n",
		"node_modules/dep.js":  "module.exports = 1",
		"docs/notes.unknownxt": "not an extension we read",
	})

	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, cursor := collect(t, s, connector.Cursor{})

	want := []string{"repo:README.md", "repo:assets/logo.png", "repo:cmd/main.go", "repo:docs/deep/nested.md", "repo:docs/install.md"}
	if !slices.Equal(ids(got), want) {
		t.Errorf("read %v\nwant %v", ids(got), want)
	}
	if cursor.IsZero() {
		t.Error("a sync that read files returned no cursor")
	}

	byID := map[string]doc.Document{}
	for _, d := range got {
		byID[d.ID] = d
	}

	if title := byID["repo:docs/install.md"].Title; title != "Installing" {
		t.Errorf("title is %q, want the markdown heading", title)
	}
	if title := byID["repo:docs/deep/nested.md"].Title; title != "no heading here, just a line" {
		t.Errorf("title is %q, want the first line when there is no heading", title)
	}
	if c := byID["repo:docs/install.md"].Container; c != "docs" {
		t.Errorf("container is %q, want the directory", c)
	}
	// A source file is named by its path. Its first line is a package comment
	// that every file in the tree shares, which would make a result list read as
	// the same title over and over.
	if title := byID["repo:cmd/main.go"].Title; title != "cmd/main.go" {
		t.Errorf("title is %q, want the path", title)
	}
	if k := byID["repo:cmd/main.go"].Kind; k != doc.KindCode {
		t.Errorf("a .go file has kind %q, want code", k)
	}
	if k := byID["repo:README.md"].Kind; k != doc.KindPage {
		t.Errorf("a .md file has kind %q, want page", k)
	}
	if p := byID["repo:cmd/main.go"].Properties["extension"]; p != "go" {
		t.Errorf("extension property is %q", p)
	}
	for id, want := range map[string]string{
		"repo:README.md":       "text/markdown",
		"repo:cmd/main.go":     "text/x-go",
		"repo:assets/logo.png": "image/png",
	} {
		if got := byID[id].Properties[doc.MediaType]; got != want {
			t.Errorf("%s has media type %q, want %q", id, got, want)
		}
	}
	if byID["repo:README.md"].ModifiedAt.IsZero() {
		t.Error("ModifiedAt was not set from the file")
	}
}

// An image is the one binary this connector reads. It has no body, because
// there is no text in it to search, and it carries its bytes and its pixel size
// so the preview can show it without guessing either.
func TestAnImageBecomesADocumentWithItsBytes(t *testing.T) {
	root := t.TempDir()
	var png bytes.Buffer
	if err := pngenc.Encode(&png, image.NewRGBA(image.Rect(0, 0, 24, 16))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "diagram.png"), png.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	// A webp is stored without dimensions, because the standard library cannot
	// read them and a decoder for one format is not worth a dependency.
	if err := os.WriteFile(filepath.Join(root, "shot.webp"), []byte("RIFF....WEBPVP8 "), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, _ := collect(t, s, connector.Cursor{})
	byID := map[string]doc.Document{}
	for _, d := range got {
		byID[d.ID] = d
	}

	d := byID["repo:diagram.png"]
	switch {
	case d.Kind != doc.KindImage:
		t.Errorf("kind is %q, want image", d.Kind)
	case d.Body != "":
		t.Errorf("an image has a body of %q, want none", d.Body)
	case d.Content == nil:
		t.Fatal("an image document carries no bytes")
	case !bytes.Equal(d.Content.Bytes, png.Bytes()):
		t.Error("the bytes are not the ones on disk")
	case d.Content.Width != 24 || d.Content.Height != 16:
		t.Errorf("size is %dx%d, want 24x16", d.Content.Width, d.Content.Height)
	}
	if w := byID["repo:shot.webp"]; w.Content == nil || w.Content.Width != 0 {
		t.Errorf("a webp should be stored with no dimensions, got %+v", w.Content)
	}
}

func TestAnOversizeImageIsSkipped(t *testing.T) {
	root := tree(t, map[string]string{
		"small.png": "\x89PNG\r\n\x1a\n",
		"huge.png":  string(make([]byte, 4096)),
	})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"), fssource.WithMaxImageSize(1024))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := collect(t, s, connector.Cursor{})
	if !slices.Equal(ids(got), []string{"repo:small.png"}) {
		t.Errorf("read %v, want only the small image", ids(got))
	}
}

func TestBinaryAndOversizeFilesAreLeftAlone(t *testing.T) {
	root := tree(t, map[string]string{
		"good.md":   "# fine\n",
		"binary.md": "\xff\xfe\x00\x01 not utf8",
		"huge.md":   string(make([]byte, 4096)),
	})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"), fssource.WithMaxFileSize(1024))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := collect(t, s, connector.Cursor{})
	if !slices.Equal(ids(got), []string{"repo:good.md"}) {
		t.Errorf("read %v, want only the good file", ids(got))
	}
}

func TestASkippedFileSaysSoRatherThanVanishing(t *testing.T) {
	root := tree(t, map[string]string{
		"good.md":   "# fine\n",
		"binary.md": "\xff\xfe\x00\x01 not utf8",
		"huge.md":   string(make([]byte, 4096)),
	})

	skipped := map[string]string{}
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"),
		fssource.WithMaxFileSize(1024),
		fssource.WithSkipped(func(path string, reason error) {
			skipped[filepath.Base(path)] = reason.Error()
		}))
	if err != nil {
		t.Fatal(err)
	}

	got, _ := collect(t, s, connector.Cursor{})
	if !slices.Equal(ids(got), []string{"repo:good.md"}) {
		t.Fatalf("read %v, want only the good file", ids(got))
	}

	// The point of the callback is that the two files nobody indexed are
	// nameable afterwards. An index missing them silently looks exactly like an
	// index that is complete.
	for _, name := range []string{"binary.md", "huge.md"} {
		if _, ok := skipped[name]; !ok {
			t.Errorf("%s was dropped without saying so, got %v", name, skipped)
		}
	}
	if len(skipped) != 2 {
		t.Errorf("skipped %v, want exactly the two that were dropped", skipped)
	}
	if want := "4096 bytes is over the limit of 1024"; skipped["huge.md"] != want {
		t.Errorf("huge.md reason is %q, want %q", skipped["huge.md"], want)
	}
}

func TestASecondSyncReadsOnlyWhatChanged(t *testing.T) {
	root := tree(t, map[string]string{
		"a.md": "# a\n",
		"b.md": "# b\n",
		"c.md": "# c\n",
	})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}

	first, cursor := collect(t, s, connector.Cursor{})
	if len(first) != 3 {
		t.Fatalf("first sync read %d files, want 3", len(first))
	}

	// Nothing changed, so nothing comes back.
	second, cursor2 := collect(t, s, cursor)
	if len(second) != 0 {
		t.Errorf("an unchanged tree produced %v", ids(second))
	}
	if cursor2.Value != cursor.Value {
		t.Errorf("the cursor moved on an unchanged tree, %q to %q", cursor.Value, cursor2.Value)
	}

	// Touch one file well into the future so the change is unambiguous on a
	// filesystem with a coarse timestamp.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(root, "b.md"), later, later); err != nil {
		t.Fatal(err)
	}
	third, _ := collect(t, s, cursor)
	if !slices.Equal(ids(third), []string{"repo:b.md"}) {
		t.Errorf("after touching one file the sync read %v", ids(third))
	}
}

func TestAnUnreadableCursorIsRefusedRatherThanIgnored(t *testing.T) {
	root := tree(t, map[string]string{"a.md": "# a\n"})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Sync(t.Context(), connector.Cursor{Value: "not-a-time"}, func(context.Context, connector.Change) error {
		t.Error("a sync with an unreadable cursor emitted a document")
		return nil
	})
	if err == nil {
		t.Fatal("an unreadable cursor was accepted, which would silently resync everything")
	}
}

// The safe default. A source with nothing to say about permissions says nothing
// rather than publishing the tree.
func TestWithoutAPolicyEverythingIsQuarantined(t *testing.T) {
	root := tree(t, map[string]string{"a.md": "# a\n", "b.md": "# b\n"})
	s, err := fssource.New(root, "repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := collect(t, s, connector.Cursor{})
	if len(got) != 2 {
		t.Fatalf("read %d documents, want 2", len(got))
	}
	for _, d := range got {
		if d.Permissions.Mode != acl.ModeUnknown {
			t.Errorf("%s has mode %v, want unknown", d.ID, d.Permissions.Mode)
		}
		// The tenant is stamped by the pipeline rather than the connector, so
		// check the descriptor directly rather than through Queryable, which
		// would be false here for the wrong reason.
		d.Tenant = "acme"
		if d.Queryable() {
			t.Errorf("%s is queryable without a policy having been given", d.ID)
		}
	}
}

// A policy that fails on one file must not publish that file and must not stop
// the walk.
func TestAPolicyErrorQuarantinesOneDocumentAndNoMore(t *testing.T) {
	root := tree(t, map[string]string{"a.md": "# a\n", "b.md": "# b\n", "c.md": "# c\n"})
	policy := fssource.PolicyFunc(func(_ context.Context, rel string) (acl.Permissions, error) {
		if rel == "b.md" {
			return acl.Permissions{}, errors.New("the share database is down")
		}
		return acl.Permissions{Mode: acl.ModePublicToTenant, Source: "repo"}, nil
	})
	s, err := fssource.New(root, "repo", policy)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := collect(t, s, connector.Cursor{})
	if len(got) != 3 {
		t.Fatalf("the walk stopped early, read %v", ids(got))
	}
	for _, d := range got {
		want := acl.ModePublicToTenant
		if d.ID == "repo:b.md" {
			want = acl.ModeUnknown
		}
		if d.Permissions.Mode != want {
			t.Errorf("%s has mode %v, want %v", d.ID, d.Permissions.Mode, want)
		}
	}
}

func TestNewRefusesWhatItCannotRead(t *testing.T) {
	root := tree(t, map[string]string{"a.md": "x"})
	if _, err := fssource.New("", "repo", nil); err == nil {
		t.Error("an empty root was accepted")
	}
	if _, err := fssource.New(root, "", nil); err == nil {
		t.Error("an empty source name was accepted")
	}
	if _, err := fssource.New(filepath.Join(root, "a.md"), "repo", nil); err == nil {
		t.Error("a file was accepted as a root")
	}
	if _, err := fssource.New(filepath.Join(root, "nope"), "repo", nil); err == nil {
		t.Error("a missing directory was accepted")
	}
}

func TestSyncStopsWhenTheCallerSaysSo(t *testing.T) {
	root := tree(t, map[string]string{"a.md": "1", "b.md": "2", "c.md": "3", "d.md": "4"})
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"))
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("enough")
	seen := 0
	_, err = s.Sync(t.Context(), connector.Cursor{}, func(context.Context, connector.Change) error {
		seen++
		if seen == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("sync returned %v, want the error the callback gave it", err)
	}
	if seen != 2 {
		t.Errorf("the walk emitted %d documents after being told to stop at 2", seen)
	}
}
