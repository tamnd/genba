package store

import (
	"context"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/doc"
)

// OpenLog is the optional capability of a driver that can remember which
// documents a person opened.
//
// It is on the server rather than in the browser because the value of the
// feature is that it follows somebody from their laptop to their desktop, and a
// list kept in local storage does not. It is optional for the same reason
// [ContentStore] is: a driver over a search service somebody else runs may have
// nowhere to put a write of its own, and refusing to implement this is a better
// answer than implementing it somewhere the next process cannot find it.
//
// The permission rule does not move. What somebody opened is not a licence to
// keep reading it, so the read applies the principal inside the driver exactly
// as a search does, and a document that stopped being visible stops being in the
// list. A record of an open is not evidence that a document exists either: a
// driver records an open only for a document the principal can see at the time,
// so writing an id nobody may read is a write that does not happen rather than
// an error that says the id was wrong.
type OpenLog interface {
	// RecordOpen notes that the principal opened a document, at the time given.
	//
	// It is idempotent per document: opening the same thing twice moves the
	// entry rather than adding a second one, because a list of the last twenty
	// things somebody looked at is not useful when nineteen of them are the same
	// document. Opening a document the principal may not read, or one that is
	// not there, records nothing and is not an error.
	RecordOpen(ctx context.Context, p *acl.Principal, id string, at time.Time) error

	// Opens returns what the principal opened, most recent first, at most limit
	// entries. A limit of zero or less returns nothing.
	Opens(ctx context.Context, p *acl.Principal, limit int) ([]Open, error)
}

// Open is one entry in an [OpenLog].
//
// It carries the document rather than its id, because every caller wants a
// title and a source to put on a row, and a list of ids that the caller then
// resolves one at a time is twenty round trips where the driver could do one.
type Open struct {
	Document doc.Document

	// At is when it was last opened.
	At time.Time
}

// OpenHistory is how many opens a driver keeps per person.
//
// It is a constant rather than an option because it is not a tuning knob: the
// list is read twenty at a time, nobody scrolls a history of their own reading
// into the hundreds, and the number exists to stop a table growing forever in a
// deployment nobody prunes. A driver drops the oldest entry past this.
const OpenHistory = 200
