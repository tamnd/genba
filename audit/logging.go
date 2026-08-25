package audit

import (
	"log/slog"
	"slices"
	"strings"
)

// Logging returns a sink that writes records to a structured logger.
//
// It is the default, and it is what makes auditing impossible to leave out
// rather than merely on by default. A deployment that has configured nothing has
// its records in whatever already collects the process log, which is not the
// retention story a compliance team wants but is a great deal better than the
// alternative, and the alternative is the reason so many systems cannot answer
// the question at all.
//
// The message is fixed and every field is a key, so the records can be filtered
// out of a mixed log by message rather than by parsing text. The document ids
// are one comma separated value: an audit record is one line, and a log where
// one access becomes eleven lines is one where the count of accesses is
// whatever the page size happened to be.
func Logging(l *slog.Logger) Sink {
	if l == nil {
		l = slog.Default()
	}
	return logging{l}
}

// Message is the message every record is logged under, so that a mixed process
// log can be filtered down to the audit trail with a string match.
const Message = "content access"

type logging struct{ log *slog.Logger }

func (s logging) Append(rec Record) error {
	attrs := []any{
		"at", rec.At,
		"tenant", rec.Tenant,
		"subject", rec.Subject,
		"surface", rec.Surface,
		"action", string(rec.Action),
		"outcome", string(rec.Outcome),
		"count", rec.Count,
	}
	if rec.Kind != "" {
		attrs = append(attrs, "kind", rec.Kind)
	}
	if rec.Query != "" {
		attrs = append(attrs, "query", rec.Query)
	}
	if len(rec.Documents) > 0 {
		attrs = append(attrs, "documents", ids(rec.Documents), "sources", sources(rec.Documents))
	}
	if rec.Rule != "" {
		attrs = append(attrs, "rule", rec.Rule)
	}
	if rec.Bytes > 0 {
		attrs = append(attrs, "bytes", rec.Bytes)
	}
	s.log.Info(Message, attrs...)
	return nil
}

// Flush and Close do nothing. A logger owns its own writer and its own
// lifetime, and a sink that closed somebody else's log on shutdown would take
// the rest of the shutdown's logging with it.
func (s logging) Flush() error { return nil }
func (s logging) Close() error { return nil }

// ids is the document ids of a record, in order.
func ids(items []Item) string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return strings.Join(out, ",")
}

// sources is which connectors they came from, deduplicated and in the order
// they first appeared. A page of eleven results from one source says "gdrive"
// rather than saying it eleven times.
func sources(items []Item) string {
	var out []string
	for _, it := range items {
		if it.Source == "" {
			continue
		}
		if !slices.Contains(out, it.Source) {
			out = append(out, it.Source)
		}
	}
	return strings.Join(out, ",")
}
