package api_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/api"
	"github.com/tamnd/genba/cache"
	"github.com/tamnd/genba/doc"
	"github.com/tamnd/genba/index"
	"github.com/tamnd/genba/store"
	"github.com/tamnd/genba/store/memstore"
)

// countingStore is a store that says how many times the bytes were read.
//
// It is the only way to tell the difference between a thumbnail that was cached
// and one that was regenerated from an identical source, and that difference is
// the whole feature: a grid that reads a megabyte off the store for every scroll
// is exactly as slow as the one this replaced.
type countingStore struct {
	store.ContentStore
	reads atomic.Int64
}

func (c *countingStore) Content(ctx context.Context, p *acl.Principal, id string) (doc.Content, error) {
	c.reads.Add(1)
	return c.ContentStore.Content(ctx, p, id)
}

// thumbnailServer holds two real images, a page with no bytes, and a file that
// is not an image at all.
func thumbnailServer(t *testing.T) (*countingStore, http.Handler) {
	t.Helper()
	st := memstore.New()
	t.Cleanup(func() { _ = st.Close() })

	perm := acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "gdrive",
		AllowGroups: []acl.Ref{{Source: "gdrive", Value: "eng@acme.com"}},
		Version:     1,
	}
	wide := paint(t, 400, 300, color.NRGBA{R: 0x20, G: 0x80, B: 0xd0, A: 0xff})
	docs := []doc.Document{
		{
			ID: "img", Tenant: "acme", Source: "gdrive", Kind: doc.KindImage,
			Title: "architecture.png", Permissions: perm,
			Properties: map[string]string{doc.MediaType: "image/png"},
			Content:    &doc.Content{Bytes: wide, Width: 400, Height: 300},
		},
		{
			ID: "tall", Tenant: "acme", Source: "gdrive", Kind: doc.KindImage,
			Title: "poster.png", Permissions: perm,
			Properties: map[string]string{doc.MediaType: "image/png"},
			Content:    &doc.Content{Bytes: paint(t, 100, 400, color.NRGBA{G: 0xa0, A: 0xff}), Width: 100, Height: 400},
		},
		{
			ID: "page", Tenant: "acme", Source: "gdrive", Kind: doc.KindPage,
			Title: "A page", Body: "text", Permissions: perm,
			Properties: map[string]string{doc.MediaType: "text/markdown"},
		},
		{
			ID: "odd", Tenant: "acme", Source: "gdrive", Kind: doc.KindFile,
			Title: "report.pdf", Permissions: perm,
			Properties: map[string]string{doc.MediaType: "application/pdf"},
			Content:    &doc.Content{Bytes: []byte("%PDF-1.7 and then some words")},
		},
	}
	if err := st.Put(t.Context(), docs...); err != nil {
		t.Fatalf("Put: %v", err)
	}

	counting := &countingStore{ContentStore: st}
	s := api.New(counting, index.New(counting), api.HeaderAuth{Tenant: "acme"})
	return counting, s.Handler()
}

func paint(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	// Noise rather than a flat fill, because a PNG of one colour compresses to a
	// few hundred bytes whatever its dimensions, and the size comparison below
	// would then be measuring the encoder rather than the scaler. The generator
	// is a plain congruential one so the fixture is the same on every run.
	seed := uint32(1)
	for y := range h {
		for x := range w {
			seed = seed*1664525 + 1013904223
			n := uint8(seed >> 24)
			img.SetNRGBA(x, y, color.NRGBA{R: c.R ^ n, G: c.G ^ n<<1, B: c.B ^ n>>1, A: c.A})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatalf("encoding a %dx%d fixture: %v", w, h, err)
	}
	return out.Bytes()
}

func TestThumbnailServesASmallPictureAtEachSize(t *testing.T) {
	_, h := thumbnailServer(t)
	sizes := map[string][2]int{
		"48":  {48, 36},
		"96":  {96, 72},
		"256": {256, 192},
	}
	for size, want := range sizes {
		w := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size="+size, engineer())
		if w.Code != http.StatusOK {
			t.Fatalf("size %s: status %d, want 200", size, w.Code)
		}
		if got := w.Header().Get("X-Content-Dimensions"); got != size+"x"+strconv.Itoa(want[1]) {
			t.Errorf("size %s: dimensions are %q, want %dx%d", size, got, want[0], want[1])
		}
		img, err := png.Decode(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatalf("size %s: the body is not a PNG: %v", size, err)
		}
		if b := img.Bounds(); b.Dx() != want[0] || b.Dy() != want[1] {
			t.Errorf("size %s: the picture is %v, want %dx%d", size, b, want[0], want[1])
		}
	}
}

// The reason the endpoint exists. A tile that costs what the original costs is
// not a tile, and the only assertion that catches a handler quietly serving the
// full image is a comparison of the two lengths.
func TestThumbnailIsMuchSmallerThanTheOriginal(t *testing.T) {
	_, h := thumbnailServer(t)
	full := request(t, h, http.MethodGet, "/api/v1/documents/img/content", engineer())
	small := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=48", engineer())

	if small.Body.Len()*8 > full.Body.Len() {
		t.Errorf("the thumbnail is %d bytes against an original of %d, which is not worth a round trip", small.Body.Len(), full.Body.Len())
	}
}

func TestThumbnailKeepsTheAspectRatioOfATallImage(t *testing.T) {
	_, h := thumbnailServer(t)
	w := request(t, h, http.MethodGet, "/api/v1/documents/tall/thumbnail?size=96", engineer())
	if got := w.Header().Get("X-Content-Dimensions"); got != "24x96" {
		t.Errorf("dimensions are %q, want 24x96", got)
	}
}

// The second request for a picture we already have must not read the bytes it
// was made from, because reading them is the cost the cache is there to avoid.
func TestThumbnailIsGeneratedOnceForAVersion(t *testing.T) {
	st, h := thumbnailServer(t)
	for range 5 {
		if w := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=48", engineer()); w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200", w.Code)
		}
	}
	if got := st.reads.Load(); got != 1 {
		t.Errorf("five requests for one thumbnail read the content %d times, want 1", got)
	}

	// A different size is a different picture, so it reads the content again,
	// and then it is cached too.
	request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=256", engineer())
	request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=256", engineer())
	if got := st.reads.Load(); got != 2 {
		t.Errorf("after a second size the content has been read %d times, want 2", got)
	}
}

// A cached thumbnail is still a document somebody may not be allowed to see, so
// the lookup that makes that decision has to happen on the warm path as well.
func TestACachedThumbnailIsStillPermissionChecked(t *testing.T) {
	_, h := thumbnailServer(t)
	if w := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=48", engineer()); w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	stranger := map[string]string{
		api.HeaderSubject: "u_kenji",
		api.HeaderGroups:  "gdrive:sales@acme.com",
	}
	w := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=48", stranger)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404, so the cache is serving a picture past the permission check", w.Code)
	}
	if strings.Contains(w.Body.String(), "PNG") {
		t.Error("the body carries image bytes")
	}
}

func TestThumbnailRefusesASizeThatIsNotOneOfThree(t *testing.T) {
	_, h := thumbnailServer(t)
	for _, size := range []string{"0", "1", "47", "64", "512", "-48", "48.5", "big"} {
		w := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size="+size, engineer())
		if w.Code != http.StatusBadRequest {
			t.Errorf("size %s returned %d, want 400", size, w.Code)
		}
	}
}

func TestThumbnailWithNoSizeIsTheLargest(t *testing.T) {
	_, h := thumbnailServer(t)
	w := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail", engineer())
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if got := w.Header().Get("X-Content-Dimensions"); got != "256x192" {
		t.Errorf("dimensions are %q, want 256x192", got)
	}
}

// Same rule as the content endpoint. A document somebody may not read, one that
// is not there, one that holds no bytes and one that is not an image are four
// different situations and one answer, because a caller who can tell them apart
// can use the difference to prove a document exists.
func TestThumbnailIsNotFoundForEverythingItWillNotRender(t *testing.T) {
	_, h := thumbnailServer(t)
	stranger := map[string]string{
		api.HeaderSubject: "u_kenji",
		api.HeaderGroups:  "gdrive:sales@acme.com",
	}
	cases := []struct {
		name    string
		target  string
		headers map[string]string
	}{
		{"a document the caller may not read", "/api/v1/documents/img/thumbnail?size=48", stranger},
		{"a document that is not there", "/api/v1/documents/nope/thumbnail?size=48", engineer()},
		{"a document that holds no bytes", "/api/v1/documents/page/thumbnail?size=48", engineer()},
		{"a document that is not an image", "/api/v1/documents/odd/thumbnail?size=48", engineer()},
	}
	var bodies []string
	for _, c := range cases {
		w := request(t, h, http.MethodGet, c.target, c.headers)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", c.name, w.Code)
		}
		bodies = append(bodies, w.Body.String())
	}
	for i, b := range bodies[1:] {
		if b != bodies[0] {
			t.Errorf("%s answers differently, which tells a caller which case they hit:\n%s\n%s", cases[i+1].name, bodies[0], b)
		}
	}
}

func TestThumbnailCarriesTheHeadersThatMakeItSafeToPutInAPage(t *testing.T) {
	_, h := thumbnailServer(t)
	w := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=96", engineer())
	for header, want := range map[string]string{
		"Content-Type":           "image/png",
		"Content-Disposition":    "inline",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "private, max-age=600",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s is %q, want %q", header, got, want)
		}
	}
	if w.Header().Get("ETag") == "" {
		t.Error("no ETag, so every scroll pays for the picture again")
	}
}

// A URL that names a version is asking for the thumbnail of that version, and
// the thumbnail of a version never changes, so the browser is told it may keep
// it without ever asking again.
func TestThumbnailOfANamedVersionIsImmutable(t *testing.T) {
	_, h := thumbnailServer(t)
	w := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=96&v=17", engineer())
	got := w.Header().Get("Cache-Control")
	if !strings.Contains(got, "immutable") || !strings.Contains(got, "private") {
		t.Errorf("Cache-Control is %q, want a private immutable response", got)
	}
}

func TestThumbnailAnswersAConditionalRequestWithoutThePicture(t *testing.T) {
	_, h := thumbnailServer(t)
	first := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=48", engineer())
	headers := engineer()
	headers["If-None-Match"] = first.Header().Get("ETag")

	w := request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=48", headers)
	if w.Code != http.StatusNotModified {
		t.Fatalf("status %d, want 304", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a 304 carried %d bytes", w.Body.Len())
	}
}

func TestStatsReportTheThumbnailCache(t *testing.T) {
	_, h := thumbnailServer(t)
	request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=48", engineer())
	request(t, h, http.MethodGet, "/api/v1/documents/img/thumbnail?size=48", engineer())

	body := decode[struct {
		Cache map[string]cache.Stats `json:"cache"`
	}](t, request(t, h, http.MethodGet, "/api/v1/stats", engineer()))

	got, ok := body.Cache["thumbnail"]
	if !ok {
		t.Fatal("stats do not report the thumbnail cache, so nobody can tell whether it is working")
	}
	if got.Hits == 0 {
		t.Errorf("the thumbnail cache reports %d hits and %d misses after the same request twice", got.Hits, got.Misses)
	}
}
