// Package ui handles terminal output and questions.
package ui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ErrAborted is returned when the user declines or interrupts.
var ErrAborted = errors.New("Abbruch durch den Benutzer")

// UI writes messages and asks questions.
type UI struct {
	Out     io.Writer
	Err     io.Writer
	Color   bool
	Yes     bool
	Verbose bool
	// ProgressTTY enables \r progress lines on Err.
	ProgressTTY bool

	in        io.Reader
	lines     chan lineResult
	startOnce sync.Once
	mu        sync.Mutex
	progress  bool
}

type lineResult struct{ s string }

// New creates a UI on the process' stdio.
func New(noColor, yes, verbose bool) *UI {
	tty := IsTerminal(os.Stdout)
	_, noColorEnv := os.LookupEnv("NO_COLOR")
	return &UI{
		Out: os.Stdout, Err: os.Stderr, in: os.Stdin,
		Color: tty && !noColor && !noColorEnv, Yes: yes, Verbose: verbose,
		ProgressTTY: IsTerminal(os.Stderr),
	}
}

// NewTest creates a UI for tests with the given input.
func NewTest(in io.Reader, out io.Writer, yes bool) *UI {
	return &UI{Out: out, Err: out, in: in, Yes: yes}
}

// IsTerminal reports whether f is a character device.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
	cDim    = "\033[2m"
)

func (u *UI) paint(code, s string) string {
	if !u.Color {
		return s
	}
	return code + s + cReset
}

func (u *UI) write(w io.Writer, s string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.progress {
		fmt.Fprint(u.Err, "\r\033[K")
		u.progress = false
	}
	fmt.Fprint(w, s)
}

// Println prints a plain line.
func (u *UI) Println(s string) { u.write(u.Out, s+"\n") }

// Printf prints a plain formatted line (newline added).
func (u *UI) Printf(format string, a ...any) { u.Println(fmt.Sprintf(format, a...)) }

// Heading prints a bold line.
func (u *UI) Heading(format string, a ...any) {
	u.Println(u.paint(cBold, fmt.Sprintf(format, a...)))
}

// Info prints an informational line.
func (u *UI) Info(format string, a ...any) { u.Println(fmt.Sprintf(format, a...)) }

// Detail prints a line only in verbose mode.
func (u *UI) Detail(format string, a ...any) {
	if u.Verbose {
		u.Println(u.paint(cDim, fmt.Sprintf(format, a...)))
	}
}

// Success prints a green line.
func (u *UI) Success(format string, a ...any) {
	u.Println(u.paint(cGreen, fmt.Sprintf(format, a...)))
}

// Warn prints a warning.
func (u *UI) Warn(format string, a ...any) {
	u.write(u.Err, u.paint(cYellow, "Warnung: "+fmt.Sprintf(format, a...))+"\n")
}

// Error prints an error.
func (u *UI) Error(format string, a ...any) {
	u.write(u.Err, u.paint(cRed, "Fehler: "+fmt.Sprintf(format, a...))+"\n")
}

// Hint prints a cyan hint.
func (u *UI) Hint(format string, a ...any) {
	u.Println(u.paint(cCyan, fmt.Sprintf(format, a...)))
}

// Progress shows a transient progress line on stderr (terminal only).
func (u *UI) Progress(s string) {
	if !u.ProgressTTY {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	fmt.Fprint(u.Err, "\r\033[K"+u.paint(cDim, s))
	u.progress = true
}

// EndProgress clears a progress line.
func (u *UI) EndProgress() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.progress {
		fmt.Fprint(u.Err, "\r\033[K")
		u.progress = false
	}
}

func (u *UI) startReader() {
	u.startOnce.Do(func() {
		u.lines = make(chan lineResult)
		go func() {
			r := bufio.NewReader(u.in)
			defer close(u.lines)
			for {
				s, err := r.ReadString('\n')
				if err != nil {
					if s != "" {
						u.lines <- lineResult{s: strings.TrimRight(s, "\r\n")}
					}
					return
				}
				u.lines <- lineResult{s: strings.TrimRight(s, "\r\n")}
			}
		}()
	})
}

// ReadLine reads one line from stdin; ctx cancellation returns ErrAborted.
func (u *UI) ReadLine(ctx context.Context) (string, error) {
	u.startReader()
	select {
	case <-ctx.Done():
		return "", ErrAborted
	case l, ok := <-u.lines:
		if !ok {
			return "", io.EOF
		}
		return l.s, nil
	}
}

// Confirm asks a yes/no question, default no. With --yes the answer is yes
// and the question is printed with the note. EOF counts as no.
func (u *UI) Confirm(ctx context.Context, question string) (bool, error) {
	if u.Yes {
		u.Println(question + " [j/N] j (--yes)")
		return true, nil
	}
	u.write(u.Out, u.paint(cBold, question+" [j/N] "))
	s, err := u.ReadLine(ctx)
	if errors.Is(err, ErrAborted) {
		u.Println("")
		return false, err
	}
	if err != nil {
		u.Println("")
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "j", "ja", "y", "yes":
		return true, nil
	}
	return false, nil
}

// ConfirmYes asks a yes/no question with default yes (Enter = yes). EOF
// counts as no.
func (u *UI) ConfirmYes(ctx context.Context, question string) (bool, error) {
	if u.Yes {
		u.Println(question + " [J/n] j (--yes)")
		return true, nil
	}
	u.write(u.Out, u.paint(cBold, question+" [J/n] "))
	s, err := u.ReadLine(ctx)
	if errors.Is(err, ErrAborted) {
		u.Println("")
		return false, err
	}
	if err != nil {
		u.Println("")
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "j", "ja", "y", "yes":
		return true, nil
	}
	return false, nil
}

// Choose asks for a number in 1..n. Empty input or EOF aborts.
func (u *UI) Choose(ctx context.Context, question string, n int) (int, error) {
	for {
		u.write(u.Out, u.paint(cBold, fmt.Sprintf("%s [1-%d, Enter = Abbruch] ", question, n)))
		s, err := u.ReadLine(ctx)
		if err != nil {
			u.Println("")
			return 0, ErrAborted
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, ErrAborted
		}
		if v, err := strconv.Atoi(s); err == nil && v >= 1 && v <= n {
			return v - 1, nil
		}
		u.Println("Ungültige Eingabe.")
	}
}

// Ask asks for a text with a default value.
func (u *UI) Ask(ctx context.Context, question, def string) (string, error) {
	if u.Yes {
		u.Printf("%s [%s] %s (--yes)", question, def, def)
		return def, nil
	}
	u.write(u.Out, u.paint(cBold, fmt.Sprintf("%s [%s] ", question, def)))
	s, err := u.ReadLine(ctx)
	if errors.Is(err, ErrAborted) {
		return "", err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return def, nil
	}
	return s, nil
}

// Pause waits for Enter (used by --pause).
func (u *UI) Pause() {
	u.write(u.Out, "\nEnter zum Schließen … ")
	_, _ = u.ReadLine(context.Background())
}

// Bytes formats a byte count for humans (decimal units, German style).
func Bytes(n int64) string {
	if n < 0 {
		return "-" + Bytes(-n)
	}
	units := []string{"B", "kB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	s := strconv.FormatFloat(f, 'f', 1, 64)
	if f >= 100 {
		s = strconv.FormatFloat(f, 'f', 0, 64)
	}
	return strings.Replace(s, ".", ",", 1) + " " + units[i]
}
