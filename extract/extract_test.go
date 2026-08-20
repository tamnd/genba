package extract

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDetectPrefersTheBytesOverTheName(t *testing.T) {
	for _, c := range []struct {
		why  string
		data string
		name string
		want string
	}{
		{"a PDF is a PDF whatever it is called", "%PDF-1.7\n1 0 obj", "notes.txt", "application/pdf"},
		{"an error page saved by a downloader that did not read the status code",
			"<!DOCTYPE html><html><body>404</body></html>", "report.pdf", "text/html"},
		{"HTML with no doctype is still HTML", "<html><head><title>x</title></head></html>", "page", "text/html"},
		{"the name decides where the bytes say nothing", "just some words", "notes.md", "text/markdown"},
		{"a source file is text, and the extension is the only evidence", "package main\n", "main.go", "text/x-go"},
		{"unnamed text is text", "an ordinary sentence", "", "text/plain"},
		{"a binary nobody has a reader for is not guessed at", "\x00\x01\x02\xff\xfe", "mystery.bin", "application/octet-stream"},
	} {
		if got := Detect([]byte(c.data), c.name); got != c.want {
			t.Errorf("%s: detected %q, want %q", c.why, got, c.want)
		}
	}
}

func TestAnUnsupportedFormatIsRefusedRatherThanScraped(t *testing.T) {
	// A binary with printable runs in it is exactly the file where scraping
	// whatever looks like text puts somebody's embedded credentials in the
	// index.
	data := append([]byte{0x00, 0x01, 0x02, 0xff}, []byte("AKIAIOSFODNN7EXAMPLE")...)

	_, err := Extract(t.Context(), bytes.NewReader(data), "keys.bin")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("error is %v, want %v", err, ErrUnsupported)
	}
}

func TestAFileOverTheInputBudgetIsRefusedWhole(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 1024)

	if _, err := Extract(t.Context(), bytes.NewReader(data), "big.txt", WithMaxInput(512)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error is %v, want %v", err, ErrTooLarge)
	}
	// A file exactly at the limit is not over it.
	if _, err := Extract(t.Context(), bytes.NewReader(data), "big.txt", WithMaxInput(1024)); err != nil {
		t.Fatalf("a file exactly at the limit: %v", err)
	}
}

func TestOutputStopsAtTheBudgetAndSaysSo(t *testing.T) {
	data := []byte(strings.Repeat("The quick brown fox. ", 1000))

	doc, err := Extract(t.Context(), bytes.NewReader(data), "long.txt", WithMaxOutput(100))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Text) > 100 {
		t.Errorf("text is %d bytes, over the budget of 100", len(doc.Text))
	}
	if !doc.Truncated {
		// Silently returning a prefix is the failure that hides: the document
		// is in the index, it looks complete, and the tail is gone everywhere.
		t.Error("the document was cut and is not marked truncated")
	}
}

// slowReader is an extractor that ignores the deadline, which is what a reader
// with a pathological loop in it looks like from outside.
type slowReader struct{}

func (slowReader) Media() []string { return []string{"text/plain"} }

func (slowReader) Extract(ctx context.Context, _ []byte, _ Options) (Doc, error) {
	<-ctx.Done()
	return Doc{Text: "half a document\n"}, nil
}

func TestAnExtractionThatRanOutOfTimeIsNotPresentedAsADocument(t *testing.T) {
	r := NewRegistry(slowReader{})

	start := time.Now()
	_, err := r.Extract(t.Context(), strings.NewReader("anything"), "notes.txt", WithTimeout(50*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error is %v, want the deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the timeout took %v to take effect", elapsed)
	}
}

// panicReader is the bug in a format reader that this package assumes exists
// somewhere.
type panicReader struct{}

func (panicReader) Media() []string { return []string{"text/plain"} }

func (panicReader) Extract(context.Context, []byte, Options) (Doc, error) {
	var s []int
	_ = s[3]
	return Doc{}, nil
}

func TestAReaderThatPanicsCostsOneDocument(t *testing.T) {
	r := NewRegistry(panicReader{})

	_, err := r.Extract(t.Context(), strings.NewReader("anything"), "notes.txt")
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("error is %v, want %v", err, ErrCorrupt)
	}
	if !strings.Contains(err.Error(), "panicked") {
		t.Errorf("the error does not say what happened: %v", err)
	}
}

// failingReader stands in for a network share that goes away mid read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestAReadThatFailsIsReported(t *testing.T) {
	_, err := Extract(t.Context(), failingReader{}, "notes.txt")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error is %v, want the read failure", err)
	}
}

// countingReader records that it was asked for the media type it claims.
type countingReader struct{ calls *int }

func (countingReader) Media() []string { return []string{"text/plain"} }

func (c countingReader) Extract(context.Context, []byte, Options) (Doc, error) {
	*c.calls++
	return Doc{Text: "from the registered reader\n"}, nil
}

func TestARegisteredReaderReplacesTheDefaultForItsType(t *testing.T) {
	calls := 0
	r := NewRegistry(Plain{})
	r.Register(countingReader{calls: &calls})

	doc, err := r.Extract(t.Context(), strings.NewReader("some text"), "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("the registered reader was called %d times, want once", calls)
	}
	if doc.Text != "from the registered reader\n" {
		t.Errorf("text is %q", doc.Text)
	}
}

func TestTheDefaultRegistryCoversTheFormatsThePackageClaims(t *testing.T) {
	for _, media := range []string{
		"text/plain", "text/csv", "text/markdown", "text/html",
		mediaWord, mediaSlides, mediaSheets, "application/pdf",
	} {
		if _, ok := Default().For(media); !ok {
			t.Errorf("no extractor for %s, which the package says it reads", media)
		}
	}
	if len(Default().Media()) < 8 {
		t.Errorf("the registry lists %d media types", len(Default().Media()))
	}
}

func TestOptionsAreAppliedOverTheDefaults(t *testing.T) {
	o := DefaultOptions()
	for _, opt := range []Option{
		WithTimeout(time.Second), WithMaxInput(1), WithMaxOutput(2), WithMaxDecompressed(3),
	} {
		opt(&o)
	}
	if o.Timeout != time.Second || o.MaxInput != 1 || o.MaxOutput != 2 || o.MaxDecompressed != 3 {
		t.Errorf("options are %+v", o)
	}
}

func TestHeadingOffsetsPointAtTheirHeadings(t *testing.T) {
	b := newBuilder(0)
	b.heading(1, "First")
	b.para("Some prose.")
	b.heading(2, "Second")
	b.para("More prose.")
	doc := b.doc()

	if len(doc.Headings) != 2 {
		t.Fatalf("got %d headings", len(doc.Headings))
	}
	for _, h := range doc.Headings {
		want := strings.Repeat("#", h.Level) + " " + h.Text
		if !strings.HasPrefix(doc.Text[h.Offset:], want) {
			t.Errorf("offset %d does not point at %q", h.Offset, want)
		}
	}
}

func TestTheOutputIsCutOnARuneBoundary(t *testing.T) {
	// Half a rune in the index is a term nothing matches and a snippet that
	// renders as a replacement character.
	b := newBuilder(10)
	b.para(strings.Repeat("é", 20))

	doc := b.doc()
	for _, r := range doc.Text {
		if r == '�' {
			t.Fatalf("the text was cut through a rune: %q", doc.Text)
		}
	}
}

func TestInvisibleCharactersAreDroppedFromWords(t *testing.T) {
	// A soft hyphen inside a word splits one term into two that nothing will
	// ever match, and it is invisible in the source and in the result.
	b := newBuilder(0)
	b.para("per\u00adformance and \ufeffthroughput")

	if got := b.doc().Text; got != "performance and throughput\n" {
		t.Errorf("text is %q", got)
	}
}
