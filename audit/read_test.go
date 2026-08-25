package audit_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/audit"
)

// trail is a directory holding one record per id, on the days given.
func trail(t *testing.T, days ...time.Time) string {
	t.Helper()
	dir := t.TempDir()
	for i, when := range days {
		f, err := audit.Open(dir, audit.WithFileClock(func() time.Time { return when }))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := f.Append(at(when, "d"+string(rune('1'+i)))); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	return dir
}

func collect(t *testing.T, dir string, from, to time.Time) []audit.Record {
	t.Helper()
	var out []audit.Record
	if err := audit.Records(dir, from, to, func(rec audit.Record) error {
		out = append(out, rec)
		return nil
	}); err != nil {
		t.Fatalf("records: %v", err)
	}
	return out
}

// TestRecordsComeBackOldestFirst, because the person reading them is
// reconstructing an afternoon and an afternoon has an order.
func TestRecordsComeBackOldestFirst(t *testing.T) {
	dir := trail(t, day(2026, 8, 26), day(2026, 8, 24), day(2026, 8, 25))

	got := collect(t, dir, time.Time{}, time.Time{})
	if len(got) != 3 {
		t.Fatalf("%d records came back, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].At.Before(got[i-1].At) {
			t.Fatalf("record %d is stamped before the one ahead of it: %v then %v", i, got[i-1].At, got[i].At)
		}
	}
}

// TestTheWindowIsInclusiveAtBothEnds. Somebody asked for the twenty fifth, and
// a report that quietly drops the last record of the day is worse than one that
// refuses.
func TestTheWindowIsInclusiveAtBothEnds(t *testing.T) {
	dir := t.TempDir()
	f, err := audit.Open(dir, audit.WithFileClock(func() time.Time { return day(2026, 8, 25) }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	start := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 25, 23, 59, 59, 0, time.UTC)
	for i, when := range []time.Time{
		start.Add(-time.Second),
		start,
		start.Add(12 * time.Hour),
		end,
		end.Add(time.Second),
	} {
		if err := f.Append(at(when, "d"+string(rune('1'+i)))); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	got := collect(t, dir, start, end)
	var ids []string
	for _, rec := range got {
		ids = append(ids, rec.Documents[0].ID)
	}
	if want := "d2 d3 d4"; strings.Join(ids, " ") != want {
		t.Errorf("the day came back as %v, want %s", ids, want)
	}
}

// TestAHalfWrittenLastLineIsTolerated. A process killed in the middle of a write
// leaves half a record at the end of the file, and refusing to read a year of
// history over it would make the trail useless exactly when somebody needs it.
func TestAHalfWrittenLastLineIsTolerated(t *testing.T) {
	dir := trail(t, day(2026, 8, 25))
	path := filepath.Join(dir, "audit-2026-08-25.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(path, slices.Concat(raw, []byte(`{"at":"2026-08-25T09:31`)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := collect(t, dir, time.Time{}, time.Time{}); len(got) != 1 {
		t.Errorf("%d records came back, want the whole one before the truncation", len(got))
	}
}

// TestABadLineInTheMiddleIsRefused, which is the other half of the same rule. A
// line with a whole record written after it is not a crash, it is corruption,
// and reading past it would report a trail with a hole in it as complete.
func TestABadLineInTheMiddleIsRefused(t *testing.T) {
	dir := trail(t, day(2026, 8, 25))
	path := filepath.Join(dir, "audit-2026-08-25.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := slices.Concat(raw, []byte("half a record\n"), raw)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err = audit.Records(dir, time.Time{}, time.Time{}, func(audit.Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "line 2 is not a record") {
		t.Errorf("a corrupt line in the middle came back as %v", err)
	}
}

// TestALineTooLongIsRefused rather than read into memory. The reader is pointed
// at a directory, and a file in it that is not one of ours should not be able to
// decide how much memory an export takes.
func TestALineTooLongIsRefused(t *testing.T) {
	dir := t.TempDir()
	line := append(bytes.Repeat([]byte("x"), audit.LineLimit+1), '\n')
	if err := os.WriteFile(filepath.Join(dir, "audit-2026-08-25.jsonl"), line, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := audit.Records(dir, time.Time{}, time.Time{}, func(audit.Record) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("a line of %d bytes came back as %v", len(line), err)
	}
}

// TestAnythingElseInTheDirectoryIsLeftAlone. A deployment that pointed this at a
// directory with other things in it should not have its export fail on a README.
func TestAnythingElseInTheDirectoryIsLeftAlone(t *testing.T) {
	dir := trail(t, day(2026, 8, 25))
	for name, body := range map[string]string{
		"README.md":             "not a record\n",
		"audit-yesterday.jsonl": "not a record either\n",
		"audit-2026-08-24.txt":  "nor this\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	if got := collect(t, dir, time.Time{}, time.Time{}); len(got) != 1 {
		t.Errorf("%d records came back, want only ours", len(got))
	}
}

// TestACallerCanStopEarly, which is how something streaming a window to a
// network writer gives up without reading the rest of a year.
func TestACallerCanStopEarly(t *testing.T) {
	dir := trail(t, day(2026, 8, 24), day(2026, 8, 25), day(2026, 8, 26))

	stop := errors.New("enough")
	seen := 0
	err := audit.Records(dir, time.Time{}, time.Time{}, func(audit.Record) error {
		seen++
		return stop
	})
	if !errors.Is(err, stop) {
		t.Errorf("the read came back with %v, want the caller's error unchanged", err)
	}
	if seen != 1 {
		t.Errorf("the read carried on for %d records after being told to stop", seen)
	}
}

// TestAMissingDirectoryIsAnError rather than an empty export, because an export
// that says nothing happened is the wrong answer to a mistyped path.
func TestAMissingDirectoryIsAnError(t *testing.T) {
	err := audit.Records(filepath.Join(t.TempDir(), "nope"), time.Time{}, time.Time{}, func(audit.Record) error {
		return nil
	})
	if err == nil {
		t.Error("a directory that is not there exported without complaint")
	}
}

// TestExportIsTheFormatItIsStoredIn. It is JSON Lines in and JSON Lines out, so
// what a compliance team is handed loads into whatever they already run.
func TestExportIsTheFormatItIsStoredIn(t *testing.T) {
	dir := trail(t, day(2026, 8, 24), day(2026, 8, 25), day(2026, 8, 26))

	var out bytes.Buffer
	n, err := audit.Export(dir, day(2026, 8, 25).Add(-9*time.Hour), time.Time{}, &out)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if n != 2 {
		t.Errorf("the export wrote %d records, want 2", n)
	}

	body := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(body) != n {
		t.Fatalf("%d records came out as %d lines", n, len(body))
	}
	var first audit.Record
	if err := json.Unmarshal([]byte(body[0]), &first); err != nil {
		t.Fatalf("the exported line is not a record: %v", err)
	}
	if first.Documents[0].ID != "d2" || first.Subject != "u_mei" {
		t.Errorf("the first exported record is %+v", first)
	}
}

// TestARecordCarriesNoContent is the promise the shape of the record makes. What
// is stored is who read what and under which rule, never a title, a snippet or a
// group name, because an audit trail readable by more people than the documents
// are is a way of leaking the documents.
func TestARecordCarriesNoContent(t *testing.T) {
	raw, err := json.Marshal(audit.Record{
		At: noon, Tenant: "acme", Subject: "u_mei", Kind: "user",
		Surface: "GET /api/v1/search", Action: audit.Search, Outcome: audit.Served,
		Query:     "payments failover",
		Documents: []audit.Item{{ID: "d1", Source: "gdrive"}},
		Count:     1, Rule: "group",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]bool{
		"at": true, "tenant": true, "subject": true, "kind": true, "surface": true,
		"action": true, "outcome": true, "query": true, "documents": true,
		"count": true, "rule": true,
	}
	for name := range fields {
		if !want[name] {
			t.Errorf("a record carries a %q field, which is not one of the ones we can defend keeping", name)
		}
	}
	documents, ok := fields["documents"].([]any)
	if !ok {
		t.Fatalf("the documents came back as %T", fields["documents"])
	}
	for _, item := range documents {
		document, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("a document came back as %T", item)
		}
		for name := range document {
			if name != "id" && name != "source" {
				t.Errorf("a document on a record carries %q, which is more than an identifier", name)
			}
		}
	}
}
