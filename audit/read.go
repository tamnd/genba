package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// LineLimit is the longest line a reader will accept.
//
// A record is a few hundred bytes and a search record with a full page of
// results on it is a few thousand, so this is far above anything this package
// writes. It is here because a reader is pointed at a directory of files, and a
// file in that directory that is not one of ours should be refused rather than
// read into memory.
const LineLimit = 1 << 20

// Records calls fn for every record written between from and to, oldest first.
//
// Both bounds are inclusive and a zero bound is open, so Records with two zero
// times is the whole directory. Files are read in date order and records within
// a file are in the order they were written, which is the order somebody
// reconstructing an afternoon needs them in.
//
// A record that fn refuses stops the read and the error comes back unchanged,
// which is how a caller streaming to a network writer stops without reading the
// rest of a year.
func Records(dir string, from, to time.Time, fn func(Record) error) error {
	files, err := files(dir, from, to)
	if err != nil {
		return err
	}
	for _, path := range files {
		if err := readFile(path, from, to, fn); err != nil {
			return err
		}
	}
	return nil
}

// Export writes the records in a window to w, in the format they are stored in.
//
// It is JSON Lines in and JSON Lines out, because the point of the export is to
// hand somebody a file their existing system can load, and a second format
// invented on the way out would be a second thing to explain. What it adds over
// copying the files is the window and the ordering.
func Export(dir string, from, to time.Time, w io.Writer) (int, error) {
	out := bufio.NewWriter(w)
	n := 0
	err := Records(dir, from, to, func(rec Record) error {
		raw, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		if _, err := out.Write(append(raw, '\n')); err != nil {
			return fmt.Errorf("audit: %w", err)
		}
		n++
		return nil
	})
	if err != nil {
		return n, err
	}
	if err := out.Flush(); err != nil {
		return n, fmt.Errorf("audit: %w", err)
	}
	return n, nil
}

// files is the audit files of a directory whose day could hold a record in the
// window, in date order.
func files(dir string, from, to time.Time) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	var out []string
	for _, entry := range entries {
		day, ok := dayOf(entry.Name())
		if !ok || entry.IsDir() {
			continue
		}
		// A whole day is skipped only when the window cannot reach into it at
		// all. The record timestamps are checked again below, because a file is
		// a day and a window is not.
		if !from.IsZero() && day.Add(24*time.Hour).Before(from.UTC()) {
			continue
		}
		if !to.IsZero() && day.After(to.UTC()) {
			continue
		}
		out = append(out, filepath.Join(dir, entry.Name()))
	}
	slices.Sort(out)
	return out, nil
}

// readFile streams one day.
func readFile(path string, from, to time.Time, fn func(Record) error) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), LineLimit)

	// A line that will not parse is held rather than reported, and reported only
	// once the next line proves it was not the last one. The last line of a file
	// a process died in the middle of is half a record, which is not corruption
	// worth refusing a year of history over, and a bad line anywhere else is.
	var (
		bad    string
		badAt  int
		number int
	)
	for scanner.Scan() {
		number++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if bad != "" {
			return fmt.Errorf("audit: %s line %d is not a record", path, badAt)
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			bad, badAt = string(line), number
			continue
		}
		if !from.IsZero() && rec.At.Before(from) {
			continue
		}
		if !to.IsZero() && rec.At.After(to) {
			continue
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		// A line too long to scan is the same class of problem as a line that
		// will not parse, and it is not the last line of a file the process
		// died in.
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("audit: %s line %d is too long to be a record", path, number+1)
		}
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}
