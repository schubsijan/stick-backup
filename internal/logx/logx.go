// Package logx writes the per-run log file on the medium.
package logx

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Log is a plain text log file. A nil *Log discards everything, which is
// used for --dry-run.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	w    *bufio.Writer
	Path string
}

// Create opens a new log file (fails if it exists).
func Create(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, w: bufio.NewWriterSize(f, 64*1024), Path: path}, nil
}

// Printf writes one timestamped line (multi-line text is indented).
func (l *Log) Printf(format string, a ...any) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, a...)
	ts := time.Now().Format("15:04:05")
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	fmt.Fprintf(l.w, "%s %s\n", ts, lines[0])
	for _, s := range lines[1:] {
		fmt.Fprintf(l.w, "         %s\n", s)
	}
}

// Raw writes a line without timestamp (rsync output).
func (l *Log) Raw(s string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.WriteString("  ")
	l.w.WriteString(s)
	l.w.WriteByte('\n')
}

// Section writes a separator with a title.
func (l *Log) Section(title string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	fmt.Fprintf(l.w, "\n== %s ==\n", title)
	l.mu.Unlock()
}

// Close flushes, fsyncs and closes the log.
func (l *Log) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.w.Flush()
	if serr := l.f.Sync(); err == nil {
		err = serr
	}
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	return err
}
