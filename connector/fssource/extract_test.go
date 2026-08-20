package fssource_test

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tamnd/genba/connector"
	"github.com/tamnd/genba/connector/fssource"
	"github.com/tamnd/genba/doc"
)

func TestADocumentIsIndexedByItsTextRatherThanItsBytes(t *testing.T) {
	// A .docx is a zip and a .pdf is a container of compressed streams. Neither
	// is text, and a connector that read them as text would put a page of
	// nothing into the index under a name somebody searches for.
	root := tree(t, map[string]string{
		"README.md": "# The project\n",
	})
	write(t, root, "reports/handbook.docx", miniDocx())
	write(t, root, "reports/memo.pdf", miniPDF())

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

	word, ok := byID["repo:reports/handbook.docx"]
	if !ok {
		t.Fatalf("the document was not read: %v", ids(got))
	}
	if !strings.Contains(word.Body, "Confirm the site contact the day before.") {
		t.Errorf("body is %q", word.Body)
	}
	// The title the author typed beats the file name, which is the whole
	// difference between a result list somebody can scan and one they cannot.
	if word.Title != "Field engineering handbook" {
		t.Errorf("title is %q", word.Title)
	}
	if want := "application/vnd.openxmlformats-officedocument.wordprocessingml.document"; word.Properties[doc.MediaType] != want {
		t.Errorf("media type is %q, want %q", word.Properties[doc.MediaType], want)
	}
	// The bytes of a document are not kept. The text is the searchable thing
	// and a preview reads the file from the source.
	if word.Content != nil {
		t.Error("the archive was stored as content")
	}

	pdf, ok := byID["repo:reports/memo.pdf"]
	if !ok {
		t.Fatalf("the PDF was not read: %v", ids(got))
	}
	if !strings.Contains(pdf.Body, "Visits were up on the quarter.") {
		t.Errorf("body is %q", pdf.Body)
	}
	// A page number is what a citation into a PDF points at, so it is recorded
	// at crawl time rather than guessed at query time.
	if pdf.Properties["pages"] != "1" {
		t.Errorf("pages is %q, want 1", pdf.Properties["pages"])
	}
	if pdf.Properties[doc.MediaType] != "application/pdf" {
		t.Errorf("media type is %q", pdf.Properties[doc.MediaType])
	}
}

func TestAHostileDocumentCostsOneDocumentRatherThanTheWalk(t *testing.T) {
	// An index quietly missing the files nobody could read looks exactly like a
	// complete one, so the skip is reported rather than swallowed.
	root := tree(t, map[string]string{
		"README.md": "# The project\n",
		"notes.md":  "# Notes\n",
	})
	broken := miniDocx()
	write(t, root, "broken.docx", broken[:len(broken)/2])

	var skipped []string
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"),
		fssource.WithSkipped(func(path string, _ error) { skipped = append(skipped, path) }))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, _ := collect(t, s, connector.Cursor{})
	if len(got) != 2 {
		t.Errorf("read %v, want the two files that are readable", ids(got))
	}
	if len(skipped) != 1 || !strings.HasSuffix(skipped[0], "broken.docx") {
		t.Errorf("skipped %v, want the one broken file", skipped)
	}
}

func TestADocumentTooLargeToExtractIsNotRead(t *testing.T) {
	// The size limit is checked against the directory entry, before anything is
	// opened, which is the only place it can be checked cheaply.
	root := tree(t, map[string]string{"README.md": "# The project\n"})
	write(t, root, "big.pdf", miniPDF())

	var skipped []string
	s, err := fssource.New(root, "repo", fssource.PublicToTenant("repo"),
		fssource.WithMaxDocumentSize(64),
		fssource.WithSkipped(func(path string, _ error) { skipped = append(skipped, path) }))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, _ := collect(t, s, connector.Cursor{})
	if len(got) != 1 {
		t.Errorf("read %v, want only the Markdown file", ids(got))
	}
	if len(skipped) != 1 {
		t.Errorf("skipped %v, want the file over the limit", skipped)
	}
}

// write puts one file into a tree, for the contents that are not text.
func write(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// miniDocx is the smallest Word document that opens, with one styled title and
// one paragraph.
func miniDocx() []byte {
	const document = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:pPr><w:pStyle w:val="Title"/></w:pPr><w:r><w:t>Field engineering handbook</w:t></w:r></w:p>
<w:p><w:r><w:t xml:space="preserve">Confirm the site contact </w:t></w:r><w:r><w:t>the day before.</w:t></w:r></w:p>
</w:body></w:document>`

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, part := range []struct{ name, body string }{
		{"[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>`},
		{"word/document.xml", document},
	} {
		f, err := w.Create(part.name)
		if err != nil {
			panic(err)
		}
		if _, err := f.Write([]byte(part.body)); err != nil {
			panic(err)
		}
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// miniPDF is one page of text drawn with a standard font.
func miniPDF() []byte {
	content := "BT\n/F1 11 Tf\n72 720 Td\n(Visits were up on the quarter.) Tj\nET\n"

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.7\n")
	for i, body := range []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> >>",
		"<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>",
		"<< /Length " + strconv.Itoa(len(content)) + " >>\nstream\n" + content + "endstream",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
	} {
		buf.WriteString(strconv.Itoa(i+1) + " 0 obj\n" + body + "\nendobj\n")
	}
	buf.WriteString("trailer\n<< /Size 6 /Root 1 0 R >>\n%%EOF\n")
	return buf.Bytes()
}
