// Package doc holds the canonical document model.
//
// Every connector normalises whatever its source calls a page, a message, a
// ticket or a file into a [Document], and everything downstream of the crawl
// works on that one shape. Keeping the model narrow is deliberate: the moment a
// ranking feature or a snippet generator starts special casing one source, the
// pipeline has to grow a branch for all of them.
package doc

import (
	"time"

	"github.com/tamnd/genba/acl"
)

// Kind is the coarse type of a document. It drives the result card layout, the
// vertical a result is blended into, and a handful of ranking features. It is
// intentionally short: fine grained types belong in Properties.
type Kind string

// The document kinds.
const (
	KindPage     Kind = "page"
	KindMessage  Kind = "message"
	KindTicket   Kind = "ticket"
	KindFile     Kind = "file"
	KindCode     Kind = "code"
	KindEmail    Kind = "email"
	KindCalendar Kind = "calendar"
	KindPerson   Kind = "person"
)

// Person is a name attached to a document, resolved to a subject where we could
// and left as a raw source identity where we could not.
type Person struct {
	Subject  string // internal subject id, empty when the identity is unresolved
	Identity acl.Identity
	Name     string
	Email    string
}

// Document is one indexable object from one source.
type Document struct {
	ID     string
	Tenant string
	Source string // the connector that produced it, for example "gdrive"
	Kind   Kind

	Title string
	Body  string
	URL   string

	Author       Person
	Owner        Person
	Container    string // the folder, space, channel or repository it lives in
	CreatedAt    time.Time
	ModifiedAt   time.Time
	IndexedAt    time.Time
	SourceUpdate string // the source's own change cursor or revision

	// Permissions is what the source said about who may read this. A document
	// whose Mode is acl.ModeUnknown is held out of every query path.
	Permissions acl.Permissions

	// Properties carries source specific fields that are worth faceting or
	// filtering on, such as a ticket status or a file's mime type.
	Properties map[string]string
}

// Queryable reports whether the document may be served to a query at all. It is
// the last gate before indexing, and the reason a permission that failed to
// resolve cannot become a search result by accident.
func (d Document) Queryable() bool {
	return d.ID != "" && d.Tenant != "" && d.Permissions.Mode != acl.ModeUnknown
}

// Chunk is a passage of a document, which is the unit that gets embedded and
// the unit an assistant cites.
type Chunk struct {
	DocID string
	Ord   int
	Text  string

	// Anchor is what a citation links to inside the document: a heading id, a
	// page number, a message timestamp or a line range.
	Anchor string
}
