# Getting the text out of a file

A connector hands the pipeline a document with a body.
For a Markdown file in a repository that is the file.
For the PDF somebody attached to a ticket, the deck a quarter was reviewed from and the spreadsheet the budget lives in, it is not, and those are the files people search for by name and cannot find.

`extract` is the package that turns those bytes into text.
It reads plain text, Markdown, HTML, Word, PowerPoint, Excel and PDF, and it is written against the standard library alone.
No office suite, no headless browser, no C library and nothing to install alongside the binary.

## One output shape

Every reader writes into the same builder and produces the same thing: a Markdown subset with headings, paragraphs, lists, tables and fenced code.

```go
doc, err := extract.Extract(ctx, file, "handbook.docx")
// doc.Title, doc.Text, doc.Headings, doc.Pages, doc.Media, doc.Truncated
```

Having one writer is what makes six readers comparable.
A heading is the same three bytes whether it came from a Word style or an `h2`, a table is written the same way whether it came from a worksheet or a `tr`, and the offsets in `doc.Headings` cannot drift out of step with the text because the same call writes both.

It is also the one place the output budget is enforced, and a limit applied in six places is a limit six readers can each forget.

## What each format is asked for

The structure worth keeping is different in every format, and taking the wrong thing is worse than taking nothing.

Word says which paragraphs are headings, in a style name.
That is the only reliable statement of structure in any of these formats: a font size is a guess and a numbered paragraph is a list rather than a section.
The style is matched loosely, because `Heading1`, `heading 1` and whatever the product that converted the file called it are all the same heading.

PowerPoint has no headings at all, so every slide gets one whether or not it had a title.
The heading is what a chunker splits on and what a citation points at, and a deck of forty untitled slides that extracted as one block would cite as "somewhere in this deck".

Excel stays a table.
Flattening the rows into prose makes every number in the file meaningless, because a figure without its row and column headings is a figure about nothing.
Cells are only written where they hold something, so the cell reference is what says which column a value is in, and a reader that ignored it would shift every value after a gap one column left.

HTML is mostly about what to throw away.
A script is code, a style sheet is not prose, an attribute that reads like text is not text, and a `title` attribute in a search result is a result nobody clicks.

Plain text is copied through unchanged, and that is the point.
The line breaks in a log file, the indentation in a Python file and the blank lines in a README are all content.
Comma separated values are the exception, and the check that decides is deliberately conservative: a table has the same number of fields on nearly every line, and in prose a comma is followed by a space.
A letter that opens "Dear team," has a comma on every line and is not a spreadsheet.

## PDF is the hard one

A PDF has no headings, no paragraphs and no sentences.
It has a page, and on that page there are instructions to draw a run of glyphs at a coordinate.
Two files that look identical in a viewer can extract as two different things, and that is a property of the format rather than a bug in the reader.

Three parts of it are worth stating.

Paragraphs come from spacing, because spacing is the only evidence there is.
Lines of running text sit one leading apart and the space around a paragraph break is bigger than that, so each gap is compared with the page's own most common gap.
A fixed threshold would be right for one type size and wrong for every other file in the corpus.

Text comes out through the font's `ToUnicode` map where the file has one.
An embedded subset font numbers its glyphs from one in the order the subsetter met them, so the codes in the content stream are not characters and cannot be guessed from anything else in the file.

Where there is no map, the result is checked before it is returned.
A page that came out as control characters and private use runes is treated as having no text at all rather than indexed, because the alternative is an index full of terms nobody can type and snippets nobody can read.

For the same reason there is no optical character recognition, so a scan extracts as nothing.
That is the honest answer.
A scanned page contains no text and inventing some would put a guess in the index with no way to tell it from a fact.
The document is still indexed: it has a name, a size, a modification time and an access control list, all of which are worth finding it by.

## A hostile file fails that file

Everything here is parsing input from outside, which is the part of a search engine most likely to be handed something built to break it.
Four things bound what one file can cost.

The archive budget is counted across the whole zip rather than per entry, because a bomb is usually a thousand small entries rather than one enormous one and a per entry limit lets every one of them through.
The declared size in a zip header is checked first and the read is limited anyway, since that number is written by whoever made the file.

The output budget is enforced in the builder, and a document that reaches it comes back with `Truncated` set rather than an error.
A prefix of a long document is worth having, and a caller that cannot tell it is a prefix would report a document as missing a phrase that is in it.

The timeout is checked between pages and between slides, and again after extraction finishes, so a document that ran out of time is never handed back as though it were whole.

The top level recovers from a panic and turns it into an error against that document.
A malformed file is one document quarantined and counted, never a worker that dies and takes a batch of ten thousand with it.

Failures are told apart, because they mean different things to whoever has to act on them.
`ErrUnsupported` is a format nobody wrote a reader for and is not a defect.
`ErrCorrupt` is a file that claims to be a format and is not, which for a half copied `.docx` means recopy it.
`ErrTooLarge` is a file over one of the budgets, which is a setting somebody may want to change.

## The corpus

The test set is a directory of real files with known content, generated by checked in Go code rather than collected.

Generating them is what makes the tests worth reading.
The files are byte identical on every machine, so a golden file that changes means the extractor changed.
There is no third party document in the repository and so no question about who owns it.
And the expected facts are written down separately from the golden output, so a reviewer looking at a diff can tell a fix from a regression.

```
go test ./extract -run TestGenerateCorpus -update
```

The corpus includes the cases that separate a real reader from a hopeful one: a paragraph split across three runs, a slide numbered ten, a worksheet reached through a relationship id that is not its position, a row with a gap in it, a PDF whose second page is compressed, one whose text is drawn with an embedded subset font, and one that is a scan.
Beside it there is a second directory of files that are meant to fail, and a test that asserts each of them fails in the way it is supposed to: a zip bomb, a truncated archive, an archive that is well formed and holds broken XML, a PDF that expands past its budget, one that is not a PDF at all, and one whose page tree points at itself.

Two of the bugs in this package were found by these tests rather than by reading the code.
Fuzzing the HTML tokenizer found NUL bytes reaching `doc.Text`, which a storage driver either refuses or silently cuts the document at, and neither failure shows up until somebody searches for the missing half.
A unit test found that the default registry was built before the table of source code media types it reads, so every Go file in a tree extracted as an unsupported format.

## Where it is wired in

`connector/fssource` reads `.pdf`, `.docx`, `.pptx` and `.xlsx` alongside the text formats it already read.
The extracted title beats the file name, the page count is recorded as a property because that is what a citation into a PDF points at, and the media type comes from the bytes rather than the extension.

Documents get their own size limit, separate from the one for text files and the one for images, because the three are about different things.
A one megabyte text file is almost certainly not prose, a one megabyte screenshot is a screenshot, and an eight megabyte PDF is an ordinary report with a chart in it.

A file that fails extraction is skipped and reported through `WithSkipped`, exactly like a file whose permission was revoked between the listing and the read.
One hostile PDF costs one document, not the hundred thousand files after it.
