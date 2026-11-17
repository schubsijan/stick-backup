// Package rsync builds rsync command lines, runs them and parses their output.
package rsync

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Binary is the rsync executable (overridable in tests).
var Binary = "rsync"

// FsyncVersion is the first rsync version with --fsync.
var FsyncVersion = [3]int{3, 2, 4}

// MinVersion is the minimum rsync version (--info=progress2, --delete-excluded
// with --from0 lists).
var MinVersion = [3]int{3, 1, 0}

// OutFormat is used for all itemized output.
const OutFormat = "%i %n%L"

// Options describe one rsync invocation.
//
// All copies use -rt with --modify-window=1, which works on exFAT/FAT/NTFS:
// only files, directories and mtimes are copied (no permissions, owners,
// hardlinks), symlinks are skipped, and mtimes compare with 1 s tolerance
// (exFAT stores 10 ms, FAT 2 s).
type Options struct {
	Source   string // directory, a trailing slash is added
	Dest     string // directory, a trailing slash is added
	Excludes []string
	// Includes, if set, turns the selection into a whitelist: only these
	// paths (relative to Source) are transferred, see IncludeRules.
	// Excludes still apply inside them.
	Includes  []string
	FilesFrom string // NUL-separated file list; disables recursion
	DryRun    bool
	Delete    bool
	// DeleteExcluded also removes excluded files from the destination.
	DeleteExcluded bool
	Update         bool
	Checksum       bool
	// Fsync makes rsync fsync every written file (rsync ≥ 3.2.4), so the
	// progress shows the real write speed of slow sticks and no gigabytes
	// pile up in the page cache.
	Fsync    bool
	Stats    bool
	Progress bool
}

// Args returns the rsync arguments (without the binary).
func Args(o Options) []string {
	a := []string{"-rt"}
	if o.DryRun {
		a = append(a, "-n")
	}
	a = append(a, "--modify-window=1")
	if o.Delete {
		a = append(a, "--delete")
	}
	if o.DeleteExcluded {
		a = append(a, "--delete-excluded")
	}
	if o.Update {
		a = append(a, "--update")
	}
	if o.Checksum {
		a = append(a, "--checksum")
	}
	if o.Fsync && !o.DryRun {
		a = append(a, "--fsync")
	}
	if o.FilesFrom != "" {
		a = append(a, "--from0", "--files-from="+o.FilesFrom)
	}
	a = append(a, filterArgs(o.Excludes, o.Includes)...)
	a = append(a, "--itemize-changes", "--out-format="+OutFormat)
	if o.Stats {
		a = append(a, "--stats", "--no-human-readable")
	}
	if o.Progress {
		a = append(a, "--info=progress2")
	}
	return append(a, withSlash(o.Source), withSlash(o.Dest))
}

func withSlash(p string) string {
	if strings.HasSuffix(p, "/") {
		return p
	}
	return p + "/"
}

// QuoteArgs renders a command line for logs.
func QuoteArgs(args []string) string {
	parts := []string{Binary}
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$*?[]{}()<>|&;#~`!") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// Handlers receive output while rsync runs. All are optional.
type Handlers struct {
	Item     func(Item)   // itemized change lines
	Progress func(string) // --info=progress2 lines
	Line     func(string) // other stdout lines (not stats)
	Stderr   func(string) // stderr lines
}

// Result of a finished rsync run.
type Result struct {
	ExitCode int
	Stats    Stats
	Stderr   []string
}

// ExitWarning reports whether the exit code only means partial transfer.
func (r *Result) ExitWarning() bool { return r.ExitCode == 23 || r.ExitCode == 24 }

// Run executes rsync. A non-zero rsync exit code is reported in Result and is
// not an error; err is set when rsync could not be started or ctx ended.
func Run(ctx context.Context, args []string, h Handlers) (*Result, error) {
	cmd := exec.CommandContext(ctx, Binary, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("rsync starten: %w", err)
	}
	res := &Result{}
	var statLines []string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanLines(stdout, func(line string) {
			switch {
			case line == "":
			case isProgress(line):
				if h.Progress != nil {
					h.Progress(strings.TrimSpace(line))
				}
			default:
				if it, ok := ParseItem(line); ok {
					if h.Item != nil {
						h.Item(it)
					}
				} else if isStatLine(line) {
					statLines = append(statLines, line)
				} else if h.Line != nil {
					h.Line(line)
				}
			}
		})
	}()
	go func() {
		defer wg.Done()
		scanLines(stderr, func(line string) {
			if line == "" {
				return
			}
			res.Stderr = append(res.Stderr, line)
			if h.Stderr != nil {
				h.Stderr(line)
			}
		})
	}()
	wg.Wait()
	werr := cmd.Wait()
	res.Stats = ParseStats(strings.Join(statLines, "\n"))
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) && ee.ExitCode() >= 0 {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, werr
	}
	return res, nil
}

// scanLines splits on \n and \r (progress output uses \r).
func scanLines(r io.Reader, fn func(string)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	for sc.Scan() {
		fn(sc.Text())
	}
	// Drain on scanner errors (overlong lines) so rsync never blocks.
	_, _ = io.Copy(io.Discard, r)
}

var progressRe = regexp.MustCompile(`^\s*[\d,.]+[KMGTP]?B?\s+\d+%`)

func isProgress(line string) bool { return progressRe.MatchString(line) }

var statPrefixes = []string{
	"Number of ", "Total file size:", "Total transferred file size:", "Literal data:",
	"Matched data:", "File list ", "Total bytes ", "sent ", "total size is ",
}

func isStatLine(line string) bool {
	for _, p := range statPrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// Item is one line of --itemize-changes output.
type Item struct {
	Flags string // 11 characters, "*deleting  " for deletions
	Path  string // relative path, directories end with "/"
	Link  string // symlink target or hardlink target, if shown
}

var itemRe = regexp.MustCompile(`^([<>ch.][fdLDS][^ ]{9}|\*deleting  ) (.+)$`)

// ParseItem parses an itemized line in the format "%i %n%L".
func ParseItem(line string) (Item, bool) {
	m := itemRe.FindStringSubmatch(line)
	if m == nil {
		return Item{}, false
	}
	it := Item{Flags: m[1], Path: m[2]}
	if it.Flags[1] == 'L' {
		if i := strings.Index(it.Path, " -> "); i >= 0 {
			it.Path, it.Link = it.Path[:i], it.Path[i+4:]
		}
	} else if it.Flags[0] == 'h' {
		if i := strings.Index(it.Path, " => "); i >= 0 {
			it.Path, it.Link = it.Path[:i], it.Path[i+4:]
		}
	}
	it.Path = unescapeName(it.Path)
	return it, true
}

// IsDelete reports a "*deleting" line.
func (i Item) IsDelete() bool { return strings.HasPrefix(i.Flags, "*deleting") }

// IsDir reports whether the item is a directory.
func (i Item) IsDir() bool {
	if i.IsDelete() {
		return strings.HasSuffix(i.Path, "/")
	}
	return i.Flags[1] == 'd'
}

// IsDirTimeOnly reports a directory whose only difference is its mtime.
func (i Item) IsDirTimeOnly() bool { return i.Flags == ".d..t......" }

// IsNew reports a newly created entry.
func (i Item) IsNew() bool { return !i.IsDelete() && strings.HasSuffix(i.Flags, "+++++++++") }

// IsTransfer reports a regular file whose data would be/was transferred.
func (i Item) IsTransfer() bool { return (i.Flags[0] == '>' || i.Flags[0] == '<') && i.Flags[1] == 'f' }

// ContentOnlyDiff reports a file whose checksum differs while size and mtime
// are equal (only meaningful with --checksum).
func (i Item) ContentOnlyDiff() bool {
	return !i.IsDelete() && !i.IsNew() && i.Flags[1] == 'f' && i.Flags[2] == 'c' && i.Flags[3] == '.' && i.Flags[4] == '.'
}

// String renders the item like rsync does.
func (i Item) String() string { return i.Flags + " " + i.Path }

var escRe = regexp.MustCompile(`\\#([0-7]{3})`)

// unescapeName decodes rsync's \#ooo escapes for non-printable characters.
func unescapeName(s string) string {
	if !strings.Contains(s, `\#`) {
		return s
	}
	return escRe.ReplaceAllStringFunc(s, func(m string) string {
		v, _ := strconv.ParseUint(m[2:], 8, 8)
		return string([]byte{byte(v)})
	})
}

// Stats holds values of --stats output.
type Stats struct {
	Files            int64 // all entries incl. directories
	Dirs             int64
	RegFiles         int64
	FilesTransferred int64 // regular files transferred
	TotalSize        int64
	TransferredSize  int64
	Valid            bool
}

// NonDirs returns the number of non-directory entries.
func (s Stats) NonDirs() int64 { return s.Files - s.Dirs }

var (
	statFilesRe = regexp.MustCompile(`(?m)^Number of files: ([\d,.]+)(?: \((.*)\))?`)
	statPartRe  = regexp.MustCompile(`(reg|dir|link|dev|special): ([\d,.]+)`)
)

func num(s string) int64 {
	s = strings.NewReplacer(",", "", ".", "", " ", "").Replace(s)
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

func statValue(out, label string) (int64, bool) {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(label) + `: ([\d,.]+)`)
	m := re.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	return num(m[1]), true
}

// ParseStats parses the output of --stats. Thousands separators are ignored.
func ParseStats(out string) Stats {
	var s Stats
	if m := statFilesRe.FindStringSubmatch(out); m != nil {
		s.Valid = true
		s.Files = num(m[1])
		for _, p := range statPartRe.FindAllStringSubmatch(m[2], -1) {
			switch p[1] {
			case "reg":
				s.RegFiles = num(p[2])
			case "dir":
				s.Dirs = num(p[2])
			}
		}
	}
	s.FilesTransferred, _ = statValue(out, "Number of regular files transferred")
	s.TotalSize, _ = statValue(out, "Total file size")
	s.TransferredSize, _ = statValue(out, "Total transferred file size")
	return s
}

// Version returns the installed rsync version.
func Version(ctx context.Context) ([3]int, string, error) {
	cmd := exec.CommandContext(ctx, Binary, "--version")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return [3]int{}, "", fmt.Errorf("rsync nicht gefunden oder nicht ausführbar: %w", err)
	}
	v, s, ok := ParseVersion(string(out))
	if !ok {
		return v, "", fmt.Errorf("rsync-Version nicht erkannt: %q", firstLine(string(out)))
	}
	return v, s, nil
}

var versionRe = regexp.MustCompile(`version\s+v?(\d+)\.(\d+)\.(\d+)(\S*)`)

// ParseVersion parses the first line of `rsync --version` (or `git --version`).
func ParseVersion(out string) ([3]int, string, bool) {
	m := versionRe.FindStringSubmatch(firstLine(out))
	if m == nil {
		return [3]int{}, "", false
	}
	var v [3]int
	for i := 0; i < 3; i++ {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, m[1] + "." + m[2] + "." + m[3] + m[4], true
}

// AtLeast compares version triples.
func AtLeast(v, min [3]int) bool {
	for i := 0; i < 3; i++ {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

var skipRe = regexp.MustCompile(`^skipping non-regular file "(.*)"$`)

// SkippedNonRegular parses rsync's message for skipped symlinks/devices.
func SkippedNonRegular(line string) (string, bool) {
	m := skipRe.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return unescapeName(m[1]), true
}

var listRe = regexp.MustCompile(`^(\S)\S*\s+[\d,.]+\s+\S+\s+\S+\s(.+)$`)

// ListFiles returns all paths (files, directories, symlinks) that rsync would
// transfer from src with the given excludes, relative to src, without ".".
func ListFiles(ctx context.Context, src string, excludes, includes []string) ([]string, error) {
	args := []string{"-r", "--list-only", "--no-human-readable"}
	args = append(args, filterArgs(excludes, includes)...)
	args = append(args, withSlash(src))
	var out []string
	res, err := Run(ctx, args, Handlers{Line: func(l string) {
		m := listRe.FindStringSubmatch(l)
		if m == nil {
			return
		}
		name := m[2]
		if m[1] == "l" {
			if i := strings.Index(name, " -> "); i >= 0 {
				name = name[:i]
			}
		}
		if name != "." {
			out = append(out, unescapeName(name))
		}
	}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 && !res.ExitWarning() {
		return nil, fmt.Errorf("rsync --list-only: Code %d: %s", res.ExitCode, strings.Join(res.Stderr, "; "))
	}
	return out, nil
}

// filterArgs returns the rsync filter options. Excludes come first so they
// also apply inside included directories (rsync: first matching rule wins).
func filterArgs(excludes, includes []string) []string {
	var a []string
	for _, e := range excludes {
		a = append(a, "--exclude="+e)
	}
	if len(includes) > 0 {
		for _, r := range IncludeRules(includes) {
			a = append(a, "--include="+r)
		}
		a = append(a, "--exclude=*")
	}
	return a
}

// IncludeRules turns whitelist paths (relative to the source, leading "/"
// optional, wildcards allowed) into anchored rsync include patterns:
// every parent directory is included on its own (so rsync descends into
// it), the path itself as file ("/p") and as directory with all contents
// ("/p/***"). A trailing "/" restricts the path to directories.
func IncludeRules(paths []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, p := range paths {
		dirOnly := strings.HasSuffix(p, "/")
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		segs := strings.Split(p, "/")
		for i := 1; i < len(segs); i++ {
			add("/" + strings.Join(segs[:i], "/") + "/")
		}
		if !dirOnly {
			add("/" + p)
		}
		add("/" + p + "/***")
	}
	return out
}
