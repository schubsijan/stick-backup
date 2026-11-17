// Package lock implements the medium lock file.
package lock

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// FileName of the lock in the medium root.
const FileName = ".backup.lock"

// Info is the JSON content of the lock file.
type Info struct {
	Host      string    `json:"host"`
	PID       int       `json:"pid"`
	User      string    `json:"user"`
	StartedAt time.Time `json:"started_at"`
	Command   string    `json:"command"`
}

// Describe renders the lock for humans.
func (i *Info) Describe() string {
	s := fmt.Sprintf("%s auf %s (PID %d, Benutzer %s) seit %s", i.Command, i.Host, i.PID, i.User, i.StartedAt.Local().Format("2006-01-02 15:04:05"))
	return s
}

// State of an existing lock relative to this process.
type State int

const (
	Foreign State = iota // other host
	Active               // same host, process alive
	Stale                // same host, process gone
)

// Lock is a held lock.
type Lock struct {
	path string
	Info Info
}

// HeldError is returned when the lock exists.
type HeldError struct {
	Info  *Info // nil if unreadable
	State State
	Err   error // read/parse error
}

func (e *HeldError) Error() string {
	if e.Info == nil {
		return fmt.Sprintf("Medium ist gesperrt (Lock-Datei unlesbar: %v)", e.Err)
	}
	return "Medium ist gesperrt: " + e.Info.Describe()
}

// Path returns the lock path in a medium.
func Path(medium string) string { return filepath.Join(medium, FileName) }

// Acquire creates the lock with O_EXCL. If it exists, a *HeldError is
// returned; host is used to classify it.
func Acquire(medium string, info Info) (*Lock, error) {
	p := Path(medium)
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, Inspect(medium, info.Host)
		}
		return nil, fmt.Errorf("Lock anlegen: %w", err)
	}
	_, werr := f.Write(append(data, '\n'))
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		os.Remove(p)
		return nil, fmt.Errorf("Lock schreiben: %w", err)
	}
	return &Lock{path: p, Info: info}, nil
}

// Release removes the lock if it is still ours.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	cur, err := Read(filepath.Dir(l.path))
	if err == nil && (cur.PID != l.Info.PID || cur.Host != l.Info.Host) {
		return errors.New("Lock gehört inzwischen einem anderen Lauf, wird nicht entfernt")
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Read reads the lock file. It returns os.ErrNotExist if there is none.
func Read(medium string) (*Info, error) {
	data, err := os.ReadFile(Path(medium))
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("Lock-Datei ungültig: %w", err)
	}
	return &info, nil
}

// Inspect classifies an existing lock. It returns nil if there is no lock.
func Inspect(medium, host string) error {
	info, err := Read(medium)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return &HeldError{State: Foreign, Err: err}
	}
	st := Foreign
	if info.Host == host {
		st = Stale
		if ProcessAlive(info.PID) {
			st = Active
		}
	}
	return &HeldError{Info: info, State: st}
}

// Remove deletes a lock file unconditionally (after confirmation).
func Remove(medium string) error { return os.Remove(Path(medium)) }

// ProcessAlive reports whether a process with this PID exists.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
