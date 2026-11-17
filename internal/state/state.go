// Package state reads and writes the status file of the mirror on the medium.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// FileName of the status file in the medium root.
const FileName = ".backup-state.json"

// FormatVersion of the status file.
const FormatVersion = 1

// Run status.
const (
	StatusRunning  = "running"
	StatusComplete = "complete"
	StatusAborted  = "aborted"
	StatusFailed   = "failed"
)

// Mapping status.
const (
	MappingOK      = "ok"
	MappingWarning = "warning"
	MappingFailed  = "failed"
	MappingSkipped = "skipped"
)

// State describes the mirror: the last run and, per mapping, which host
// updated it last.
type State struct {
	FormatVersion int        `json:"format_version"`
	ToolVersion   string     `json:"tool_version"`
	MediumID      string     `json:"medium_id"`
	Host          string     `json:"host"` // host of the last run
	User          string     `json:"user"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Status        string     `json:"status"`
	Mappings      []Mapping  `json:"mappings"`
	Warnings      []string   `json:"warnings"`
}

// Mapping is the state of one mapping in the mirror.
type Mapping struct {
	Name             string     `json:"name"`
	Source           string     `json:"source"`
	Target           string     `json:"target"`
	Filter           string     `json:"filter,omitempty"`
	Host             string     `json:"host"` // host that last updated it
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
	Status           string     `json:"status"`
	FilesTotal       int64      `json:"files_total"`
	FilesTransferred int64      `json:"files_transferred"`
	BytesTransferred int64      `json:"bytes_transferred"`
	FilesDeleted     int64      `json:"files_deleted"`
	Warnings         []string   `json:"warnings"`
}

// Succeeded reports whether the mapping was copied (possibly with warnings).
func (m *Mapping) Succeeded() bool { return m.Status == MappingOK || m.Status == MappingWarning }

// Mapping returns the entry for a target, or nil.
func (s *State) Mapping(target string) *Mapping {
	for i := range s.Mappings {
		if filepath.Clean(s.Mappings[i].Target) == target {
			return &s.Mappings[i]
		}
	}
	return nil
}

// WarningCount counts run and mapping warnings.
func (s *State) WarningCount() int {
	n := len(s.Warnings)
	for _, m := range s.Mappings {
		n += len(m.Warnings)
	}
	return n
}

// Read reads the status file; it returns (nil, nil) if there is none.
func Read(medium string) (*State, error) {
	data, err := os.ReadFile(filepath.Join(medium, FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s := &State{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return s, nil
}

// Write writes the status file atomically and fsyncs it.
func Write(medium string, s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(medium, FileName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(data, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(medium, FileName))
}

// SyncDir fsyncs a directory.
func SyncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

var hostCleanRe = regexp.MustCompile(`[^a-zA-Z0-9-]`)

// CleanHost reduces a host name to [a-zA-Z0-9-] (used in log names).
func CleanHost(h string) string {
	h = hostCleanRe.ReplaceAllString(h, "-")
	if h == "" {
		return "unknown"
	}
	return h
}
