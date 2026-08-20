package connectortest

import (
	"fmt"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector"
)

// problems reports everything wrong with one change, and nothing at all for a
// well formed one.
//
// It is a plain function over a value rather than something reaching for a
// [testing.T] so that it can be tested directly, which matters more here than
// anywhere else in this package. A check that quietly passed everything would
// look exactly like a suite that everything passes, and the only way to tell
// those apart is to hand this a broken change and watch it complain.
func problems(ch connector.Change, source string) []string {
	var out []string
	d := ch.Document

	if d.ID == "" {
		out = append(out, "a change carries no document id, so nothing above the connector can tell which document it is about")
	}
	if ch.Deleted && ch.PermissionsOnly {
		out = append(out, d.ID+" is reported as deleted and as a permission change at the same time, and those ask for opposite things")
	}
	if d.Tenant != "" {
		// The tenant is the pipeline's to set, from the run rather than from the
		// source. A connector that fills it in is one whose documents land in
		// whichever tenant it was first written for, and the pipeline overwrites
		// it anyway, so the only thing the field can do here is mislead whoever
		// reads the connector next.
		out = append(out, fmt.Sprintf("%s carries tenant %q, which is set by the pipeline rather than by a connector", d.ID, d.Tenant))
	}
	if d.Source != "" && d.Source != source {
		out = append(out, fmt.Sprintf("%s carries source %q, which is not %q", d.ID, d.Source, source))
	}
	if ch.PermissionsOnly && (d.Body != "" || d.Content != nil) {
		// Nothing above reads content off a permissions only change, so a
		// connector sending one is either wasting the read or expecting an
		// update that will not happen.
		out = append(out, d.ID+" is a permission change and carries content, which will not be stored")
	}

	if ch.Deleted {
		// There is nothing left to say about a document that is gone. Asking a
		// connector to resolve the permissions of a file it can no longer read
		// would be asking for a guess.
		return out
	}

	switch p := d.Permissions; {
	case p.Source == "":
		// This is the rule the whole suite exists for. A document indexed
		// without the access control list that governs it is searchable by
		// everybody, and no later fix makes it unsearchable by the people who
		// have already found it. A connector that considered the question and
		// failed says so with connector.Unresolved and the document is
		// quarantined instead.
		out = append(out, fmt.Sprintf("%s arrived without permissions: a connector that cannot work out who may read a document says so with connector.Unresolved(%q) rather than leaving the field empty", d.ID, source))
	case p.Source != source:
		out = append(out, fmt.Sprintf("%s has permissions from source %q, and this connector is %q", d.ID, p.Source, source))
	}

	switch d.Permissions.Mode {
	case acl.ModeUnknown, acl.ModeACL, acl.ModePublicToTenant:
	default:
		out = append(out, fmt.Sprintf("%s reports permission mode %d, which is not one this system has", d.ID, int(d.Permissions.Mode)))
	}
	return out
}
