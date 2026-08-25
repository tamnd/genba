package audit_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/audit"
)

// at is a record stamped for a given moment, which is what decides the file it
// lands in.
func at(when time.Time, id string) audit.Record {
	rec := read("u_mei", id)
	rec.At = when
	return rec
}

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 9, 30, 0, 0, time.UTC)
}

// TestADayIsAFile. Everything anybody does with these files is by date, so the
// day is in the name and a day's records are all in one place.
func TestADayIsAFile(t *testing.T) {
	dir := t.TempDir()
	f, err := audit.Open(dir, audit.WithFileClock(func() time.Time { return day(2026, 8, 25) }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	for _, rec := range []audit.Record{
		at(day(2026, 8, 25), "d1"),
		at(day(2026, 8, 25).Add(2*time.Hour), "d2"),
		at(day(2026, 8, 26), "d3"),
	} {
		if err := f.Append(rec); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := lines(t, filepath.Join(dir, "audit-2026-08-25.jsonl")); len(got) != 2 {
		t.Errorf("the first day holds %d records, want 2", len(got))
	}
	second := lines(t, filepath.Join(dir, "audit-2026-08-26.jsonl"))
	if len(second) != 1 {
		t.Fatalf("the second day holds %d records, want 1", len(second))
	}
	var rec audit.Record
	if err := json.Unmarshal([]byte(second[0]), &rec); err != nil {
		t.Fatalf("the line is not a record: %v", err)
	}
	if rec.Documents[0].ID != "d3" {
		t.Errorf("the record after the roll is %+v", rec)
	}
}

// TestNobodyElseCanReadTheTrail. A record says which documents a named person
// read, and a directory of those is worth reading for somebody who wanted to
// know what a company is working on.
func TestNobodyElseCanReadTheTrail(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	f, err := audit.Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != audit.DirMode {
		t.Errorf("the directory is %v, want %v", perm, audit.DirMode)
	}
	file, err := os.Stat(f.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := file.Mode().Perm(); perm != audit.FileMode {
		t.Errorf("the file is %v, want %v", perm, audit.FileMode)
	}
}

// TestARestartAppendsToTheDay rather than starting it again. A process that is
// restarted twice in an afternoon should not lose the morning.
func TestARestartAppendsToTheDay(t *testing.T) {
	dir := t.TempDir()
	clock := func() time.Time { return day(2026, 8, 25) }

	for _, id := range []string{"d1", "d2"} {
		f, err := audit.Open(dir, audit.WithFileClock(clock))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := f.Append(at(day(2026, 8, 25), id)); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	if got := lines(t, filepath.Join(dir, "audit-2026-08-25.jsonl")); len(got) != 2 {
		t.Errorf("the day holds %d records after a restart, want 2", len(got))
	}
}

// TestADirectoryThatCannotBeWrittenIsAStartupFailure, rather than something the
// first person to search for anything discovers.
func TestADirectoryThatCannotBeWrittenIsAStartupFailure(t *testing.T) {
	if _, err := audit.Open(" "); err == nil {
		t.Error("an empty directory opened without complaint")
	}
	if os.Geteuid() == 0 {
		// Permissions do not apply to root, and a container that runs its tests
		// as root would fail this for a reason that has nothing to do with us.
		t.Skip("running as root")
	}
	dir := filepath.Join(t.TempDir(), "closed")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := audit.Open(filepath.Join(dir, "audit")); err == nil {
		t.Error("a directory nobody can write to opened without complaint")
	}
}

// TestRetentionDeletesWhatAgedOutAndNothingElse. A file is deleted only once
// the whole day it holds is outside the window, so two days of retention at
// half past nine keeps the day before yesterday rather than deleting records
// that are thirty hours old, and a file in the directory that is not ours is
// left where it is.
func TestRetentionDeletesWhatAgedOutAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"audit-2026-08-20.jsonl",
		"audit-2026-08-23.jsonl",
		"audit-2026-08-24.jsonl",
		"audit-2026-08-25.jsonl",
		"README.md",
		"audit-not-a-date.jsonl",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	f, err := audit.Open(dir,
		audit.WithFileClock(func() time.Time { return day(2026, 8, 25) }),
		audit.WithRetention(48*time.Hour))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	want := []string{
		"README.md",
		"audit-2026-08-23.jsonl",
		"audit-2026-08-24.jsonl",
		"audit-2026-08-25.jsonl",
		"audit-not-a-date.jsonl",
	}
	if got := names(t, dir); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the directory holds %v, want %v", got, want)
	}
}

// TestNothingIsDeletedWithoutARetention. A deployment that has not said how long
// it keeps its audit trail has not decided, and deleting somebody's compliance
// record on a guess is not a default this gets to pick.
func TestNothingIsDeletedWithoutARetention(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "audit-2019-01-01.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f, err := audit.Open(dir, audit.WithFileClock(func() time.Time { return day(2026, 8, 25) }))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := names(t, dir); len(got) != 2 {
		t.Errorf("the directory holds %v, want the old file kept", got)
	}
}

// TestTheDayIsRolledIntoRetention. Pruning runs when the file rolls, which is
// the only moment a long lived process has to notice that another day has aged
// out of the window.
func TestTheDayIsRolledIntoRetention(t *testing.T) {
	dir := t.TempDir()
	now := day(2026, 8, 25)
	f, err := audit.Open(dir,
		audit.WithFileClock(func() time.Time { return now }),
		audit.WithRetention(24*time.Hour))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := f.Append(at(day(2026, 8, 25), "d1")); err != nil {
		t.Fatalf("append: %v", err)
	}

	now = day(2026, 8, 27)
	if err := f.Append(at(now, "d2")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	want := []string{"audit-2026-08-27.jsonl"}
	if got := names(t, dir); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("the directory holds %v after the roll, want %v", got, want)
	}
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	body := strings.TrimSuffix(string(raw), "\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out
}
