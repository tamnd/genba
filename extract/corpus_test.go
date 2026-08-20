package extract

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The corpus test.
//
// Every extractor here is a guess about somebody else's format, and the only
// way to know whether a guess still holds is to run it over files and compare
// the result with what the files are known to say. Unit tests catch the case a
// reader was written for. A corpus catches the case it was not: the paragraph
// split across three runs, the spreadsheet with a gap in a row, the slide with
// no title, the font whose codes are its own.
//
// The expected text is a golden file rather than a list of assertions, because
// the thing worth protecting is the whole output. A test that only checks for
// a phrase passes just as happily when the headings have gone missing and the
// table has collapsed into a line of numbers.

var update = flag.Bool("update", false, "rebuild the corpus files and the expected extractions")

const (
	corpusDir  = "testdata/corpus"
	hostileDir = "testdata/hostile"
	goldenDir  = "testdata/corpus/golden"
)

// known is what each corpus file says, written down independently of what the
// extractor happens to produce.
//
// The golden files are regenerated with -update and would happily record a
// regression as the new truth. These facts are not: they are here so that a
// reviewer who accepts a golden diff without reading it still has to explain
// why the title went missing.
var known = []struct {
	file     string
	media    string
	title    string
	pages    int
	headings []string
	contains []string
	absent   []string
}{
	{
		file:     "handbook.md",
		media:    "text/markdown",
		title:    "Field engineering handbook",
		headings: []string{"Field engineering handbook", "Before the visit", "On site"},
		contains: []string{"Charge both batteries.", "ssh field@edge-07"},
		absent:   []string{"owner: field-ops"},
	},
	{
		file:     "notes.txt",
		media:    "text/plain",
		contains: []string{"Ingest is behind by about four hours", "Ask the platform team"},
	},
	{
		file:     "people.csv",
		media:    "text/csv",
		contains: []string{"| name | team | site |", "| Amara Nwosu | Field ops | Manchester |"},
	},
	{
		file:     "report.html",
		media:    "text/html",
		title:    "Quarterly field report",
		headings: []string{"Quarterly field report", "Sites visited", "Open issues"},
		contains: []string{
			"the & in this sentence is an entity",
			"| Manchester | 14 |",
			"- Edge 07 still reports a failed disk.",
			"journalctl -u genbad --since yesterday",
		},
		absent: []string{"should not be indexed", "font-family"},
	},
	{
		file:     "handbook.docx",
		media:    mediaWord,
		title:    "Field engineering handbook",
		headings: []string{"Field engineering handbook", "Before the visit", "Parts to carry", "On site"},
		contains: []string{
			// The paragraph is three runs in the file and one sentence on the
			// page, which is the single most common thing a naive reader gets
			// wrong.
			"Confirm the site contact the day before.",
			"- Charge both batteries.",
			"| Battery | 2 |",
		},
	},
	{
		file:     "review.pptx",
		media:    mediaSlides,
		title:    "Quarter in review",
		pages:    3,
		headings: []string{"Quarter in review", "What went well", "Slide 3"},
		contains: []string{"Two new sites onboarded", "An untitled closing slide"},
	},
	{
		file:     "budget.xlsx",
		media:    mediaSheets,
		title:    "Field budget",
		headings: []string{"Travel", "Equipment"},
		contains: []string{
			"| Site | Flights | Hotel |",
			// Berlin has no flights cell at all, and the column it does have
			// has to stay in the column it belongs to.
			"| Berlin |  | 280 |",
			"| Spare battery | 89 |",
		},
	},
	{
		file:  "memo.pdf",
		media: "application/pdf",
		title: "Quarterly memo",
		pages: 2,
		contains: []string{
			"Quarterly memo",
			"Manchester ran fourteen visits and Berlin ran nine.",
			// The upper half of the font's encoding, which a reader that
			// treats the bytes as characters turns into control codes.
			"read as – and “this”.",
			// Two strings with a wide gap between them are two words.
			"Edge 07 still reports a failed disk.",
		},
	},
	{
		file:  "subset.pdf",
		media: "application/pdf",
		title: "Embedded subset fonts",
		pages: 1,
		contains: []string{
			"Embedded subset fonts",
			"Only the ToUnicode map says what they are.",
		},
	},
	{
		file:  "scan.pdf",
		media: "application/pdf",
		pages: 1,
		// A scan holds no text and this package does not invent any.
		absent: []string{"a", "e"},
	},
}

func TestCorpus(t *testing.T) {
	if *update {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range known {
		t.Run(want.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(corpusDir, want.file))
			if err != nil {
				t.Fatalf("%v, regenerate the corpus with: go test ./extract -run TestGenerateCorpus -update", err)
			}

			doc, err := Extract(t.Context(), bytes.NewReader(data), want.file)
			if err != nil {
				t.Fatalf("extracting: %v", err)
			}

			if doc.Media != want.media {
				t.Errorf("media is %q, want %q", doc.Media, want.media)
			}
			if doc.Title != want.title {
				t.Errorf("title is %q, want %q", doc.Title, want.title)
			}
			if doc.Pages != want.pages {
				t.Errorf("pages is %d, want %d", doc.Pages, want.pages)
			}
			if doc.Bytes != int64(len(data)) {
				t.Errorf("read %d bytes, want %d", doc.Bytes, len(data))
			}
			if doc.Truncated {
				t.Error("document is marked truncated, and none of the corpus is near the budget")
			}

			var got []string
			for _, h := range doc.Headings {
				got = append(got, h.Text)
				if h.Offset < 0 || h.Offset >= len(doc.Text) {
					t.Errorf("heading %q has offset %d, outside text of %d bytes", h.Text, h.Offset, len(doc.Text))
					continue
				}
				// The offset is what a citation anchors to, so it has to point
				// at the heading rather than near it.
				if !strings.HasPrefix(doc.Text[h.Offset:], strings.Repeat("#", h.Level)+" "+h.Text) {
					t.Errorf("heading %q does not start at offset %d", h.Text, h.Offset)
				}
			}
			if strings.Join(got, "|") != strings.Join(want.headings, "|") {
				t.Errorf("headings are %q, want %q", got, want.headings)
			}

			for _, s := range want.contains {
				if !strings.Contains(doc.Text, s) {
					t.Errorf("text does not contain %q", s)
				}
			}
			for _, s := range want.absent {
				if strings.Contains(doc.Text, s) {
					t.Errorf("text contains %q, which should not have been extracted", s)
				}
			}

			golden(t, goldenName(want.file), doc.Text)
		})
	}
}

// goldenName is what a corpus file's expected extraction is called. Every
// extraction is Markdown, and the source extension stays in the name because
// the same document is in the corpus twice in two formats.
func goldenName(file string) string {
	if strings.HasSuffix(file, ".md") {
		return file
	}
	return file + ".md"
}

// golden compares the extraction with the file recording what it produced last
// time, and rewrites that file under -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v, rebuild the expected output with: go test ./extract -update", err)
	}
	if got != string(want) {
		t.Errorf("extraction changed\n got: %q\nwant: %q", got, string(want))
	}
}

// hostile is what each file built to break a reader should do instead.
//
// None of them may take the process down, and none of them may hang. What they
// return is the interesting part: a file over budget is refused, and a file
// that is merely broken gives up whatever it managed to read, because half a
// contract still answers somebody's question.
var hostile = []struct {
	file    string
	wantErr error
	partial string
}{
	{file: "bomb.docx", wantErr: ErrTooLarge},
	{file: "truncated.docx", wantErr: ErrCorrupt},
	{file: "malformed.docx", partial: "The first paragraph survives."},
	{file: "empty.docx"},
	{file: "bomb.pdf", wantErr: ErrTooLarge},
	{file: "garbage.pdf", wantErr: ErrCorrupt},
	{file: "cycle.pdf", partial: "Round and round."},
}

func TestHostileFilesFailOneDocumentAndNothingElse(t *testing.T) {
	for _, want := range hostile {
		t.Run(want.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(hostileDir, want.file))
			if err != nil {
				t.Fatalf("%v, regenerate the corpus with: go test ./extract -run TestGenerateCorpus -update", err)
			}

			// The budgets are small on purpose. A bomb is a bomb relative to
			// what it was given, and a test that has to allocate the default
			// two hundred and fifty six megabytes to prove it is a test nobody
			// runs on a laptop.
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			doc, err := Extract(ctx, bytes.NewReader(data), want.file,
				WithMaxDecompressed(1<<20), WithMaxOutput(1<<20), WithTimeout(10*time.Second))

			switch {
			case want.wantErr != nil:
				if !errors.Is(err, want.wantErr) {
					t.Fatalf("error is %v, want %v", err, want.wantErr)
				}
			case err != nil:
				t.Fatalf("extracting: %v", err)
			case want.partial != "" && !strings.Contains(doc.Text, want.partial):
				t.Errorf("text does not contain %q, which the file did manage to say:\n%s", want.partial, doc.Text)
			}
		})
	}
}

// TestEveryHostileFileIsCovered catches the file added to the directory and
// never asserted about, which is a corpus file that tests nothing.
func TestEveryHostileFileIsCovered(t *testing.T) {
	entries, err := os.ReadDir(hostileDir)
	if err != nil {
		t.Fatal(err)
	}
	covered := make(map[string]bool, len(hostile))
	for _, h := range hostile {
		covered[h.file] = true
	}
	for _, e := range entries {
		if !covered[e.Name()] {
			t.Errorf("%s is in the hostile corpus and nothing says what it should do", e.Name())
		}
	}
}

// TestEveryCorpusFileIsCovered is the same check for the well formed files.
func TestEveryCorpusFileIsCovered(t *testing.T) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	covered := make(map[string]bool, len(known))
	for _, k := range known {
		covered[k.file] = true
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !covered[e.Name()] {
			t.Errorf("%s is in the corpus and nothing says what it should extract as", e.Name())
		}
	}
}
