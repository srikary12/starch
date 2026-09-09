package prompt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Presets on disk.
//
// The file is meant to be edited by hand — the brief is explicit that this is
// a first-class path, not a hidden file people reverse-engineer. Three things
// follow from that. It is written out with the defaults on first use, so there
// is something to look at. It is re-read when it changes, so an edit takes
// effect without restarting anything. And a broken file never stops a rewrite:
// the defaults stand in, and the problem is reported rather than swallowed.

// FileName is the on-disk name inside the support directory.
const FileName = "presets.json"

// Source says where the active set came from.
type Source string

const (
	// SourceFile: loaded from presets.json.
	SourceFile Source = "file"
	// SourceDefaults: the built-in set, because there is no file or it could
	// not be read.
	SourceDefaults Source = "defaults"
)

// Set is the active preset list plus where it came from.
type Set struct {
	Presets []Preset `json:"presets"`
	Path    string   `json:"path"`
	Source  Source   `json:"source"`
	// Problem describes why the file was not used, when it exists but could
	// not be read. Empty otherwise.
	//
	// Reported rather than logged and forgotten: someone who has just edited
	// this file and sees no change needs to be told their JSON is broken, or
	// they will conclude the feature does not work.
	Problem string `json:"problem,omitempty"`
}

// fileFormat is the on-disk shape. An object rather than a bare array so the
// format has somewhere to grow without breaking every existing file.
type fileFormat struct {
	Presets []Preset `json:"presets"`
}

// Store loads and saves the preset file, caching between reads.
type Store struct {
	path string

	mu      sync.Mutex
	cached  []Preset
	problem string
	source  Source
	// stamp identifies the file version the cache was built from.
	stamp fileStamp
	// loaded guards against treating a zero stamp as "already read".
	loaded bool
}

// fileStamp is a cheap identity for the file's contents.
//
// Modification time plus size, rather than hashing: this is checked on every
// rewrite, and reading a whole file to decide whether to read it would be a
// strange thing to do inside a latency budget.
type fileStamp struct {
	modTime int64
	size    int64
	exists  bool
}

// NewStore returns a store backed by presets.json in dir.
func NewStore(dir string) *Store {
	return &Store{path: filepath.Join(dir, FileName)}
}

// Path is the file this store reads and writes.
func (s *Store) Path() string { return s.path }

// Load returns the active preset set.
//
// Cheap to call per request: it stats the file and only re-parses when
// something changed, so editing presets.json takes effect on the next rewrite
// with no restart and no file watcher.
func (s *Store) Load() Set {
	s.mu.Lock()
	defer s.mu.Unlock()

	stamp := statFile(s.path)
	if s.loaded && stamp == s.stamp {
		return s.set()
	}

	s.stamp = stamp
	s.loaded = true
	s.problem = ""

	if !stamp.exists {
		// Write the defaults so there is something to edit. A failure here is
		// not fatal — the defaults still apply in memory — so it is recorded
		// and the rewrite proceeds.
		s.cached = clone(Defaults)
		s.source = SourceDefaults
		if err := writeFile(s.path, s.cached); err != nil {
			s.problem = fmt.Sprintf("could not create %s: %v", s.path, err)
		} else {
			// The file now matches the cache; take its stamp so the write we
			// just made does not read back as an external change.
			s.stamp = statFile(s.path)
			s.source = SourceFile
		}
		return s.set()
	}

	presets, err := readFile(s.path)
	if err != nil {
		// Deliberately keeps serving the defaults. Someone mid-sentence should
		// get a rewrite, not an error about a config file.
		s.cached = clone(Defaults)
		s.source = SourceDefaults
		s.problem = err.Error()
		return s.set()
	}

	s.cached = presets
	s.source = SourceFile
	return s.set()
}

// Save validates and writes a preset set, replacing the file.
func (s *Store) Save(presets []Preset) error {
	if err := Validate(presets); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := writeFile(s.path, presets); err != nil {
		return err
	}
	s.cached = clone(presets)
	s.source = SourceFile
	s.problem = ""
	s.stamp = statFile(s.path)
	s.loaded = true
	return nil
}

// set builds the return value. Caller holds the lock.
func (s *Store) set() Set {
	return Set{
		Presets: clone(s.cached),
		Path:    s.path,
		Source:  s.source,
		Problem: s.problem,
	}
}

// Validate reports whether a preset set is usable.
var (
	ErrNoPresets       = errors.New("at least one preset is required")
	ErrDuplicateID     = errors.New("preset ids must be unique")
	ErrIncompleteEntry = errors.New("every preset needs an id, a name and an instruction")
)

// MaxPresets bounds a set. Well past anything anyone will cycle through with
// Tab, and low enough that a malformed file cannot exhaust memory.
const MaxPresets = 64

func Validate(presets []Preset) error {
	if len(presets) == 0 {
		return ErrNoPresets
	}
	if len(presets) > MaxPresets {
		return fmt.Errorf("at most %d presets are allowed, got %d", MaxPresets, len(presets))
	}

	seen := make(map[string]bool, len(presets))
	for i, p := range presets {
		if strings.TrimSpace(p.ID) == "" ||
			strings.TrimSpace(p.Name) == "" ||
			strings.TrimSpace(p.Instruction) == "" {
			return fmt.Errorf("%w (entry %d)", ErrIncompleteEntry, i+1)
		}
		if seen[p.ID] {
			return fmt.Errorf("%w: %q appears twice", ErrDuplicateID, p.ID)
		}
		seen[p.ID] = true
	}
	return nil
}

func readFile(path string) ([]Preset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", filepath.Base(path), err)
	}

	var parsed fileFormat
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// json.SyntaxError carries a byte offset, which is useless on its own;
		// a line and column is what a person editing the file can act on.
		return nil, fmt.Errorf("%s is not valid JSON: %s", filepath.Base(path), describeJSONError(raw, err))
	}
	if err := Validate(parsed.Presets); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return parsed.Presets, nil
}

func writeFile(path string, presets []Preset) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(fileFormat{Presets: presets}, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	// Written via a temporary file and renamed, so an interrupted write cannot
	// leave a half-written file that then fails to parse on next launch.
	temp, err := os.CreateTemp(filepath.Dir(path), ".presets-*.json")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, 0o600); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// describeJSONError turns a byte offset into a line and column.
func describeJSONError(raw []byte, err error) string {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError

	offset := int64(-1)
	switch {
	case errors.As(err, &syntax):
		offset = syntax.Offset
	case errors.As(err, &typeErr):
		offset = typeErr.Offset
	}
	if offset < 0 || offset > int64(len(raw)) {
		return err.Error()
	}

	line := 1 + strings.Count(string(raw[:offset]), "\n")
	column := int(offset)
	if nl := strings.LastIndexByte(string(raw[:offset]), '\n'); nl >= 0 {
		column = int(offset) - nl
	}
	return fmt.Sprintf("%v (line %d, column %d)", err, line, column)
}

func statFile(path string) fileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{modTime: info.ModTime().UnixNano(), size: info.Size(), exists: true}
}

func clone(presets []Preset) []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	return out
}
