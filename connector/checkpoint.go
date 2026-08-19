package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// MemoryCheckpoints keeps resume points in a map.
//
// It is what tests use and what a single run that has no reason to resume can
// use. It survives nothing, which is the point: a deployment that wants to
// resume after a restart has to say where the checkpoints live.
type MemoryCheckpoints struct {
	mu sync.RWMutex
	at map[string]Cursor
}

// NewMemoryCheckpoints returns an empty checkpoint store.
func NewMemoryCheckpoints() *MemoryCheckpoints {
	return &MemoryCheckpoints{at: make(map[string]Cursor)}
}

var _ Checkpoints = (*MemoryCheckpoints)(nil)

// Load returns the resume point for a source.
func (m *MemoryCheckpoints) Load(ctx context.Context, tenant, source string) (Cursor, error) {
	if err := ctx.Err(); err != nil {
		return Cursor{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.at[tenant+"\x00"+source], nil
}

// Save records a resume point.
func (m *MemoryCheckpoints) Save(ctx context.Context, tenant, source string, c Cursor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.at[tenant+"\x00"+source] = c
	return nil
}

// FileCheckpoints keeps one small JSON file per tenant and source in a
// directory.
//
// A file is the right shape for this. Checkpoints are written once per batch,
// read once per run, and are worthless to anybody but the connector that wrote
// them, so putting them in the content database buys nothing and couples the
// two.
type FileCheckpoints struct {
	dir string
	mu  sync.Mutex
}

// NewFileCheckpoints returns a checkpoint store rooted at dir, creating it if it
// does not exist.
func NewFileCheckpoints(dir string) (*FileCheckpoints, error) {
	if dir == "" {
		return nil, errors.New("connector: checkpoint directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("connector: checkpoint directory: %w", err)
	}
	return &FileCheckpoints{dir: dir}, nil
}

var _ Checkpoints = (*FileCheckpoints)(nil)

// checkpointFile is the on disk shape. It is JSON because an operator reading a
// stuck sync should be able to see the answer with cat.
type checkpointFile struct {
	Tenant string    `json:"tenant"`
	Source string    `json:"source"`
	Value  string    `json:"value"`
	Time   time.Time `json:"time"`
}

// Load returns the resume point for a source, or a zero cursor if it has never
// run.
func (f *FileCheckpoints) Load(ctx context.Context, tenant, source string) (Cursor, error) {
	if err := ctx.Err(); err != nil {
		return Cursor{}, err
	}
	b, err := os.ReadFile(f.path(tenant, source))
	if os.IsNotExist(err) {
		// A source that has never been synced is the normal first run, not a
		// failure to report.
		return Cursor{}, nil
	}
	if err != nil {
		return Cursor{}, fmt.Errorf("connector: load checkpoint: %w", err)
	}
	var cf checkpointFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return Cursor{}, fmt.Errorf("connector: parse checkpoint for %s: %w", source, err)
	}
	return Cursor{Value: cf.Value, Time: cf.Time}, nil
}

// Save records a resume point.
//
// It writes a temporary file and renames it over the target, because rename is
// the one filesystem operation that is atomic on every platform this runs on. A
// crash in the middle leaves either the old checkpoint or the new one. The
// alternative, writing in place, can leave a half written cursor that resumes
// past documents that were never indexed, and nothing later would notice.
func (f *FileCheckpoints) Save(ctx context.Context, tenant, source string, c Cursor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(checkpointFile{Tenant: tenant, Source: source, Value: c.Value, Time: c.Time})
	if err != nil {
		return fmt.Errorf("connector: encode checkpoint: %w", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	final := f.path(tenant, source)
	tmp, err := os.CreateTemp(f.dir, ".checkpoint-*")
	if err != nil {
		return fmt.Errorf("connector: save checkpoint: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // a no op once the rename below has succeeded

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("connector: save checkpoint: %w", err)
	}
	// Sync before the rename. Without it the rename can reach the disk before
	// the bytes do, which turns a crash into a checkpoint that points somewhere
	// real and contains nothing.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("connector: save checkpoint: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("connector: save checkpoint: %w", err)
	}
	if err := os.Rename(name, final); err != nil {
		return fmt.Errorf("connector: save checkpoint: %w", err)
	}
	return nil
}

// path is the file a tenant and source pair is stored in. The name is escaped
// so that a source called "a/b" cannot write outside the directory.
func (f *FileCheckpoints) path(tenant, source string) string {
	return filepath.Join(f.dir, escape(tenant)+"_"+escape(source)+".json")
}

// escape maps anything that is not a plain name character to a hyphen. It is
// not reversible and does not need to be: the file records the tenant and
// source it belongs to inside itself.
func escape(s string) string {
	if s == "" {
		return "-"
	}
	out := []byte(s)
	for i, c := range out {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
		if !ok {
			out[i] = '-'
		}
	}
	return string(out)
}
