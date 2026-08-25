package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Prefix and Extension are how an audit file is named. A day's records are in
// audit-2026-08-25.jsonl, in UTC.
//
// The name carries the date rather than a sequence number, because everything
// anybody does with these files is by date: ship yesterday's, keep ninety days,
// find the week of the incident. A rotation scheme with numbers in it makes all
// three of those a script.
const (
	Prefix    = "audit-"
	Extension = ".jsonl"

	// Layout is the date in a file name, which is the same order the name sorts
	// in, which is why it is this one.
	Layout = "2006-01-02"
)

// DirMode and FileMode are what the directory and the files are created with.
//
// They are narrow because of what is in them. A record says which documents a
// named person read, and a directory of those is worth reading for somebody who
// wanted to know what a company is working on, so it is not group readable and
// it is certainly not world readable.
const (
	DirMode  fs.FileMode = 0o700
	FileMode fs.FileMode = 0o600
)

// File is a sink writing JSON Lines to one file per day.
//
// One record per line, one file per UTC day, no header and no framing. That is
// the format because it is the one every log shipper, every warehouse loader and
// every command line already reads: an export is cp, and an ingest is whatever
// the company already runs. A format of ours would need a tool of ours, and the
// team that has to answer for this in an audit does not want a tool of ours.
type File struct {
	dir       string
	retention time.Duration
	now       func() time.Time

	mu   sync.Mutex
	day  string
	file *os.File
	buf  *bufio.Writer
}

// FileOption configures a [File].
type FileOption func(*File)

// WithRetention deletes files older than d when the day rolls over.
//
// Zero keeps everything, which is the default, because a deployment that has
// not said how long it keeps its audit trail has not decided, and deleting
// somebody's compliance record on a guess is not a default this gets to pick.
func WithRetention(d time.Duration) FileOption {
	return func(f *File) {
		if d > 0 {
			f.retention = d
		}
	}
}

// WithFileClock sets the clock used to decide what is old enough to delete.
func WithFileClock(now func() time.Time) FileOption {
	return func(f *File) {
		if now != nil {
			f.now = now
		}
	}
}

// Open returns a sink writing into a directory, creating it if it is not there.
func Open(dir string, opts ...FileOption) (*File, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("audit: no directory")
	}
	f := &File{dir: dir, now: time.Now}
	for _, opt := range opts {
		opt(f)
	}
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	// Opened now rather than on the first record, so that a directory nobody can
	// write to is a startup failure rather than a discovery made by the first
	// person to search for something.
	if err := f.open(f.now().UTC().Format(Layout)); err != nil {
		return nil, err
	}
	f.prune()
	return f, nil
}

// Append writes one record.
func (f *File) Append(rec Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if day := rec.At.UTC().Format(Layout); day != f.day {
		if err := f.roll(day); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	// One write of one line. A record split across two writes is a line a reader
	// cannot parse if the process dies between them, and the whole value of this
	// format is that a truncated file is still readable up to the truncation.
	raw = append(raw, '\n')
	if _, err := f.buf.Write(raw); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// Flush pushes the buffer to the file.
//
// It does not fsync. A record is durable against the process dying, which is
// what the log is for, and not against the machine losing power in the same
// second, which would cost a disk write per search. See docs/audit.md.
func (f *File) Flush() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flush()
}

// Close flushes and closes the current file.
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.file == nil {
		return nil
	}
	err := f.flush()
	if cerr := f.file.Close(); cerr != nil && err == nil {
		err = fmt.Errorf("audit: %w", cerr)
	}
	f.file, f.buf = nil, nil
	return err
}

// Path is the file being written, which is what a startup line says so that
// nobody has to guess where the records went.
func (f *File) Path() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return filepath.Join(f.dir, Prefix+f.day+Extension)
}

// roll moves to another day's file and prunes what has aged out.
func (f *File) roll(day string) error {
	if f.file != nil {
		if err := f.flush(); err != nil {
			return err
		}
		if err := f.file.Close(); err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		f.file, f.buf = nil, nil
	}
	if err := f.open(day); err != nil {
		return err
	}
	f.prune()
	return nil
}

// open appends to one day's file, creating it if this is the first record of
// the day and reopening it if the process was restarted during it.
func (f *File) open(day string) error {
	file, err := os.OpenFile(filepath.Join(f.dir, Prefix+day+Extension),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, FileMode)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	f.day, f.file, f.buf = day, file, bufio.NewWriter(file)
	return nil
}

func (f *File) flush() error {
	if f.buf == nil {
		return nil
	}
	if err := f.buf.Flush(); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}

// prune deletes what has aged out of the retention window.
//
// Failures are ignored on purpose. This runs on the writer, once a day, and a
// file that could not be deleted is a disk that will fill up eventually, which
// is a slower and more visible problem than an audit log that stopped writing
// because a stale file was in the way.
func (f *File) prune() {
	if f.retention <= 0 {
		return
	}
	cutoff := f.now().UTC().Add(-f.retention)
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		day, ok := dayOf(entry.Name())
		if !ok || entry.IsDir() {
			continue
		}
		// The end of the day rather than the start, so a retention of a day
		// keeps today and yesterday whole rather than deleting records written
		// this morning.
		if day.Add(24 * time.Hour).After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(f.dir, entry.Name()))
	}
}

// dayOf reads the date out of an audit file name, and says no to anything else
// in the directory. A deployment that pointed this at a directory with other
// things in it should lose nothing but its own old records.
func dayOf(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, Prefix) || !strings.HasSuffix(name, Extension) {
		return time.Time{}, false
	}
	day, err := time.Parse(Layout, strings.TrimSuffix(strings.TrimPrefix(name, Prefix), Extension))
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}
