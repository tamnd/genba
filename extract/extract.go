// Package extract turns a file into the text a search engine can index.
//
// Extraction quality sets the ceiling on search quality. A document whose text
// never came out of it cannot be ranked, cannot be snippeted and cannot be
// cited, and no amount of work further down the pipeline recovers it. That is
// the whole reason this package exists as its own layer rather than as a helper
// inside one connector: the same PDF arrives from a file share, an object
// store and a chat attachment, and it should come out the same way each time.
//
// # One shape out
//
// Every extractor produces the same thing: text with its headings still in it,
// written in a small subset of Markdown, plus an outline in [Doc.Headings] of
// where those headings are.
//
//	doc, err := extract.Extract(ctx, f, "handbook.docx")
//	doc.Text      // "# Onboarding\n\nDay one is...\n"
//	doc.Headings  // [{Level: 1, Text: "Onboarding", Offset: 0}]
//
// Markdown is the canonical form because chunking has to split a document
// somewhere, and the only good places to split are the boundaries the author
// wrote. A Word heading, an HTML h2 and a spreadsheet's sheet name are all the
// same fact once they are on the page, and turning each of them into a
// different structure would mean every consumer downstream handling all of
// them. Tables survive for the same reason: a row that has lost its columns is
// a sentence nobody wrote.
//
// # Nothing here can stop a run
//
// Files arrive from places nobody controls. A crawl that dies on one malformed
// spreadsheet has lost the other ninety thousand documents in the batch, so
// every failure in this package is scoped to one document. A parser that
// panics on a hostile file is turned into an error, because a bug in a format
// reader is a matter of when rather than whether, and the blast radius of one
// is the thing that is actually worth controlling.
//
// The budgets are the other half of that. A file that decompresses to a
// hundred gigabytes and a file that takes an hour to parse both take a whole
// crawler down with them, and neither is hypothetical: the first is a zip bomb
// and the second is any sufficiently large table.
//
// # What is not here
//
// There is no optical character recognition, so an image is an image and a
// scanned PDF extracts as nothing rather than as a guess.
//
// A format nobody has written a reader for returns [ErrUnsupported] rather than
// its own bytes. Indexing the internals of an unknown binary would put whatever
// happens to be printable in it into the index, and the first thing that lands
// there is somebody's embedded credentials.
package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Doc is one extracted document.
type Doc struct {
	// Title is what the file called itself, and is empty when it said nothing.
	// It is not derived from the file name here, because the connector knows
	// the name and this package only ever sees the bytes.
	Title string

	// Text is the extracted content in the Markdown subset described on the
	// package: ATX headings, blank line separated paragraphs, "- " list items
	// and pipe tables.
	Text string

	// Headings is where the structure went, in the order it appears, with byte
	// offsets into Text. It is what a chunker splits on and what an anchor in a
	// citation is built from.
	Headings []Heading

	// Media is the media type the file turned out to be, which is not always
	// the one its name claimed.
	Media string

	// Pages is how many pages a paginated format had, and zero for the formats
	// that have no such thing.
	Pages int

	// Truncated says the output hit [Options.MaxOutput] and the tail of the
	// document is missing. It is worth carrying rather than logging because a
	// missing tail is invisible in a search result: the document is there, it
	// looks complete, and a query for something in the last chapter finds
	// nothing.
	Truncated bool

	// Bytes is how much input was read.
	Bytes int64
}

// Heading is one heading and where it sits in [Doc.Text].
type Heading struct {
	Level  int
	Text   string
	Offset int
}

// The errors extraction fails with. Each of them means the document is not
// indexable and the crawl carries on, and they are separate because they call
// for different reactions: an unsupported format is a gap in this package, a
// corrupt file is a fact about the file, and a file over budget is a decision
// somebody made about the budget.
var (
	// ErrUnsupported is a format no extractor here handles.
	ErrUnsupported = errors.New("extract: unsupported format")

	// ErrCorrupt is a file that claims a format it does not hold up as.
	ErrCorrupt = errors.New("extract: corrupt file")

	// ErrTooLarge is a file over [Options.MaxInput], or one that decompresses
	// past [Options.MaxDecompressed].
	ErrTooLarge = errors.New("extract: file over budget")
)

// Options is the budget one extraction runs under.
//
// The defaults are sized for a crawler running several extractions at once on
// an ordinary machine, which is the case worth defaulting for. A deployment
// that indexes engineering drawings will want them larger and knows it.
type Options struct {
	// Timeout bounds one extraction. A file that takes longer is abandoned.
	Timeout time.Duration

	// MaxInput is the most that will be read from one file. Reading stops
	// there and the extraction fails rather than working on a prefix, because
	// half a zip archive and half a PDF are not documents.
	MaxInput int64

	// MaxOutput is the most text one document produces. Output stops there and
	// the document is marked truncated, which is the opposite decision from
	// MaxInput and deliberately so: the first half of a log file is a usable
	// document and the first half of a container is not.
	MaxOutput int64

	// MaxDecompressed is the most a compressed container may expand to in
	// total. It is what stops a zip bomb, where a megabyte on disk becomes
	// however much memory the reader is willing to give it.
	MaxDecompressed int64
}

// DefaultOptions is the budget an extraction runs under when nobody says
// otherwise.
func DefaultOptions() Options {
	return Options{
		Timeout:         30 * time.Second,
		MaxInput:        32 << 20,
		MaxOutput:       4 << 20,
		MaxDecompressed: 256 << 20,
	}
}

// An Option adjusts the budget.
type Option func(*Options)

// WithTimeout bounds one extraction.
func WithTimeout(d time.Duration) Option { return func(o *Options) { o.Timeout = d } }

// WithMaxInput sets how much of a file will be read.
func WithMaxInput(n int64) Option { return func(o *Options) { o.MaxInput = n } }

// WithMaxOutput sets how much text one document may produce.
func WithMaxOutput(n int64) Option { return func(o *Options) { o.MaxOutput = n } }

// WithMaxDecompressed sets how far a compressed container may expand.
func WithMaxDecompressed(n int64) Option { return func(o *Options) { o.MaxDecompressed = n } }

// An Extractor reads one family of formats.
//
// It is handed the whole file rather than a reader, because every format here
// except plain text needs to seek: a zip archive keeps its index at the end,
// and a PDF is a graph of objects addressed by byte offset. The bytes are
// already bounded by [Options.MaxInput] before an extractor sees them, so the
// simplification does not cost a memory bound.
type Extractor interface {
	// Media returns the media types this extractor handles.
	Media() []string

	// Extract turns the file into a document. It must not panic and it must
	// respect the budget, and the wrapper in this package assumes neither.
	Extract(ctx context.Context, data []byte, o Options) (Doc, error)
}

// Registry is the set of extractors in use.
//
// A deployment that has a format nobody here has written a reader for
// registers its own rather than forking this package, which is why the
// interface is exported at all.
type Registry struct {
	by map[string]Extractor
}

// NewRegistry returns a registry holding the extractors given.
func NewRegistry(es ...Extractor) *Registry {
	r := &Registry{by: make(map[string]Extractor, len(es)*2)}
	for _, e := range es {
		r.Register(e)
	}
	return r
}

// Register adds an extractor for every media type it claims, replacing any
// extractor already registered for one.
func (r *Registry) Register(e Extractor) {
	for _, m := range e.Media() {
		r.by[m] = e
	}
}

// For returns the extractor for a media type.
func (r *Registry) For(media string) (Extractor, bool) {
	e, ok := r.by[media]
	return e, ok
}

// Media returns the media types the registry can extract, for a health
// endpoint or a log line that says what this build understands.
func (r *Registry) Media() []string {
	out := make([]string, 0, len(r.by))
	for m := range r.by {
		out = append(out, m)
	}
	return out
}

// defaultRegistry is what [Extract] uses.
//
// It is built on first use rather than at initialisation because what an
// extractor claims to read can itself be a table in this package, and the order
// package level variables are initialised in is not something a reader of
// either file would think to check. Building it lazily makes the question not
// arise.
var defaultRegistry = sync.OnceValue(func() *Registry {
	return NewRegistry(
		Plain{},
		Markdown{},
		HTML{},
		Word{},
		Slides{},
		Sheets{},
		PDF{},
	)
})

// Default returns a registry holding every extractor this package ships.
func Default() *Registry { return defaultRegistry() }

// Extract reads a file and returns its text, using the default registry.
//
// The name is used to tell a Markdown file from a plain one and little else.
// What the file actually is comes from its first bytes, because a name is a
// claim and the bytes are the fact, and the two disagree often enough that
// trusting the name means occasionally handing a PDF to the text reader.
func Extract(ctx context.Context, r io.Reader, name string, opts ...Option) (Doc, error) {
	return Default().Extract(ctx, r, name, opts...)
}

// Extract reads a file and returns its text.
func (r *Registry) Extract(ctx context.Context, src io.Reader, name string, opts ...Option) (Doc, error) {
	o := DefaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	data, err := read(src, o.MaxInput)
	if err != nil {
		return Doc{}, err
	}

	media := Detect(data, name)
	e, ok := r.For(media)
	if !ok {
		return Doc{}, fmt.Errorf("%w: %s", ErrUnsupported, media)
	}

	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}

	doc, err := run(ctx, e, data, o)
	if err != nil {
		return Doc{}, fmt.Errorf("extract %s: %w", media, err)
	}
	doc.Media = media
	doc.Bytes = int64(len(data))
	return doc, nil
}

// run calls an extractor and turns a panic into an error.
//
// The recover is not defensive habit. These parsers read files that arrive
// from outside, in formats defined by other people and produced by software
// nobody here has seen, and an index out of range in one of them is a question
// of when. Without this, one such file ends a crawl of a hundred thousand
// documents and the operator's evidence is a stack trace naming a byte offset.
// With it, one document is lost and named.
func run(ctx context.Context, e Extractor, data []byte, o Options) (doc Doc, err error) {
	defer func() {
		if r := recover(); r != nil {
			doc = Doc{}
			err = fmt.Errorf("%w: reader panicked: %v", ErrCorrupt, r)
		}
	}()
	doc, err = e.Extract(ctx, data, o)
	if err != nil {
		return Doc{}, err
	}
	// A reader that stopped early because the deadline passed has produced a
	// prefix, and a prefix presented as a whole document is the failure that
	// hides: the document is in the index, it looks complete, and the half that
	// is missing is missing everywhere.
	if err := ctx.Err(); err != nil {
		return Doc{}, err
	}
	return doc, nil
}

// read pulls a file into memory, refusing one over the limit.
//
// It reads one byte past the limit on purpose. A file exactly at the limit is
// fine and a file over it has to be told apart from one that happens to end
// there, and the alternative is trusting a size somebody reported.
func read(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultOptions().MaxInput
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("extract: read: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: over %d bytes", ErrTooLarge, limit)
	}
	return data, nil
}

// Detect works out what a file is, from its first bytes and then its name.
//
// The order matters. A name is what somebody called the file and the bytes are
// what it is, and the cases where they differ are the interesting ones: a
// report saved as notes.txt, an HTML error page saved with a .pdf extension by
// a downloader that did not check the status code.
func Detect(data []byte, name string) string {
	if m := sniff(data); m != "" {
		return m
	}
	if m := byExtension(strings.ToLower(path.Ext(name))); m != "" {
		return m
	}
	// Nothing recognised the bytes and nothing recognised the name. Valid UTF-8
	// is text, and anything else is a binary this package will not guess at.
	if utf8.Valid(data) {
		return "text/plain"
	}
	return "application/octet-stream"
}

// MediaByName is what a file's name claims it is, and empty when the name
// claims nothing this package recognises.
//
// [Detect] is the better answer and this is not a substitute for it. It exists
// for the callers that have to decide whether to read a file at all before they
// have a single byte of it: a connector reading a bucket over the network
// cannot sniff its way through a terabyte of archives to find out none of them
// were documents.
func MediaByName(name string) string {
	return byExtension(strings.ToLower(path.Ext(name)))
}

// magic is one file signature.
type magic struct {
	prefix []byte
	media  string
}

// signatures are the formats that announce themselves in their first bytes.
//
// The zip signature covers every Office format at once, because all of them
// are zip archives, so a second pass over the archive's contents decides which
// one it is. That is in [zipMedia] rather than here.
var signatures = []magic{
	{[]byte("%PDF-"), "application/pdf"},
	{[]byte("\x89PNG\r\n\x1a\n"), "image/png"},
	{[]byte("\xff\xd8\xff"), "image/jpeg"},
	{[]byte("GIF87a"), "image/gif"},
	{[]byte("GIF89a"), "image/gif"},
	{[]byte("%!PS"), "application/postscript"},
	{[]byte("\x7fELF"), "application/x-executable"},
	{[]byte("\xd0\xcf\x11\xe0"), "application/x-ole-storage"},
}

// sniff returns the media type the bytes announce, or empty.
func sniff(data []byte) string {
	for _, s := range signatures {
		if bytes.HasPrefix(data, s.prefix) {
			return s.media
		}
	}
	if m := zipMedia(data); m != "" {
		return m
	}
	if isHTML(data) {
		return "text/html"
	}
	return ""
}

// byExtension is the fallback for formats that have no signature worth
// trusting, which is every text format.
func byExtension(ext string) string {
	switch ext {
	case ".md", ".markdown", ".mdown":
		return "text/markdown"
	case ".html", ".htm", ".xhtml":
		return "text/html"
	case ".txt", ".text", ".log", ".rst", ".adoc":
		return "text/plain"
	case ".csv":
		return "text/csv"
	case ".json", ".yaml", ".yml", ".toml", ".ini", ".xml":
		return "text/plain"
	case ".pdf":
		return "application/pdf"
	case ".docx", ".docm":
		return mediaWord
	case ".pptx", ".pptm":
		return mediaSlides
	case ".xlsx", ".xlsm":
		return mediaSheets
	}
	if m := codeMedia(ext); len(m) == 1 {
		return m[0]
	}
	return ""
}
