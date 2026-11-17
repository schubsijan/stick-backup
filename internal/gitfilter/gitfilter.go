// Package gitfilter builds the file list for mappings with filter = "git".
//
// Git is only used to select files: every repository below the mapping root
// is evaluated with its own ignore rules, in an isolated environment that
// does not depend on the user's or system's git configuration.
package gitfilter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/schubsijan/stick-backup/internal/rsync"
)

// Binary is the git executable.
var Binary = "git"

// MinVersion: GIT_CONFIG_GLOBAL exists since git 2.32.
var MinVersion = [3]int{2, 32, 0}

// Version checks that git is installed and new enough.
func Version(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, Binary, "--version")
	cmd.Env = env()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git nicht gefunden oder nicht ausführbar: %w", err)
	}
	v, s, ok := rsync.ParseVersion(string(out))
	if !ok {
		return "", fmt.Errorf("git-Version nicht erkannt: %q", strings.TrimSpace(string(out)))
	}
	if !rsync.AtLeast(v, MinVersion) {
		return s, fmt.Errorf("git %s ist zu alt, benötigt wird mindestens %d.%d", s, MinVersion[0], MinVersion[1])
	}
	return s, nil
}

// Options for BuildList.
type Options struct {
	Root     string   // mapping source, absolute and resolved
	Excludes []string // gitignore syntax, applied in every repository
	Includes []string // gitignore syntax, files added despite ignore rules
	GitDirs  bool     // add .git of every repository
	TmpDir   string   // private temporary directory (not on the medium)
}

// RepoInfo is logged per evaluated repository.
type RepoInfo struct {
	Path        string // relative to Root, "." for Root
	IsRepo      bool
	Files       int
	IncludeTime time.Duration
}

// Result of BuildList.
type Result struct {
	Files    []string // sorted, unique, relative to Root; files and symlinks only
	Repos    []RepoInfo
	Warnings []string
}

// env returns the environment for git: no GIT_* from the caller, no system
// or global config.
func env() []string {
	var out []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "GIT_") || strings.HasPrefix(e, "LC_ALL=") {
			continue
		}
		out = append(out, e)
	}
	return append(out, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

type builder struct {
	ctx      context.Context
	opt      Options
	excludes string
	includes string
	helper   string
	files    map[string]bool
	res      *Result
}

// BuildList walks all repositories below Root and returns the file list.
func BuildList(ctx context.Context, opt Options) (*Result, error) {
	b := &builder{ctx: ctx, opt: opt, files: map[string]bool{}, res: &Result{}}
	b.excludes = filepath.Join(opt.TmpDir, "excludes")
	// Always set core.excludesFile, even when empty: otherwise git falls back
	// to ~/.config/git/ignore although GIT_CONFIG_GLOBAL=/dev/null.
	if err := os.WriteFile(b.excludes, []byte(strings.Join(opt.Excludes, "\n")+"\n"), 0o600); err != nil {
		return nil, err
	}
	if len(opt.Includes) > 0 {
		b.includes = filepath.Join(opt.TmpDir, "includes")
		if err := os.WriteFile(b.includes, []byte(strings.Join(opt.Includes, "\n")+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	if err := b.walk("."); err != nil {
		return nil, err
	}
	for f := range b.files {
		b.res.Files = append(b.res.Files, f)
	}
	sort.Strings(b.res.Files)
	return b.res, nil
}

func isRepoRoot(abs string) bool {
	_, err := os.Lstat(filepath.Join(abs, ".git"))
	return err == nil
}

func (b *builder) git(dir string, isRepo bool, args ...string) ([]byte, error) {
	base := []string{"-c", "core.excludesFile=" + b.excludes, "-c", "core.fsmonitor=false"}
	if !isRepo {
		if b.helper == "" {
			h := filepath.Join(b.opt.TmpDir, "helper.git")
			cmd := exec.CommandContext(b.ctx, Binary, "init", "--quiet", "--bare", h)
			cmd.Env = env()
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("Hilfs-Repo anlegen: %v: %s", err, bytes.TrimSpace(out))
			}
			b.helper = h
		}
		base = append(base, "--git-dir="+b.helper, "--work-tree="+dir)
	}
	cmd := exec.CommandContext(b.ctx, Binary, append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if b.ctx.Err() != nil {
			return nil, b.ctx.Err()
		}
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func splitZ(out []byte) []string {
	var r []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			r = append(r, string(p))
		}
	}
	return r
}

// add records rel if it is a regular file or symlink. It returns whether rel
// is a directory that is a repository root.
func (b *builder) add(rel string) (subRepo bool) {
	abs := filepath.Join(b.opt.Root, rel)
	fi, err := os.Lstat(abs)
	if err != nil {
		return false // tracked but deleted, or vanished
	}
	switch {
	case fi.Mode().IsRegular() || fi.Mode()&fs.ModeSymlink != 0:
		b.files[rel] = true
	case fi.IsDir():
		return isRepoRoot(abs)
	}
	return false
}

func (b *builder) walk(rel string) error {
	abs := filepath.Join(b.opt.Root, rel)
	isRepo := isRepoRoot(abs)
	info := RepoInfo{Path: rel, IsRepo: isRepo}
	before := len(b.files)

	out, err := b.git(abs, isRepo, "ls-files", "-co", "--exclude-standard", "-z")
	if err != nil {
		if rel == "." || errors.Is(err, context.Canceled) {
			return err
		}
		b.res.Warnings = append(b.res.Warnings, fmt.Sprintf("Unter-Repo %s übersprungen: %v", rel, err))
		return nil
	}
	var subs []string
	for _, e := range splitZ(out) {
		r := filepath.Join(rel, strings.TrimSuffix(e, "/"))
		if b.add(r) {
			subs = append(subs, r)
		}
	}

	if b.includes != "" {
		start := time.Now()
		out, err := b.git(abs, isRepo, "ls-files", "-o", "-i", "--exclude-from="+b.includes, "-z")
		info.IncludeTime = time.Since(start)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			b.res.Warnings = append(b.res.Warnings, fmt.Sprintf("include in %s: %v", rel, err))
		} else {
			for _, e := range splitZ(out) {
				b.add(filepath.Join(rel, strings.TrimSuffix(e, "/")))
			}
		}
	}

	if isRepo && b.opt.GitDirs {
		gitRel := filepath.Join(rel, ".git")
		err := filepath.WalkDir(filepath.Join(b.opt.Root, gitRel), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				b.res.Warnings = append(b.res.Warnings, fmt.Sprintf(".git lesen: %v", err))
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0 {
				r, _ := filepath.Rel(b.opt.Root, p)
				b.files[r] = true
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	info.Files = len(b.files) - before
	b.res.Repos = append(b.res.Repos, info)

	for _, s := range subs {
		if err := b.walk(s); err != nil {
			return err
		}
	}
	return nil
}

// WriteList writes the list NUL-separated for rsync --from0 --files-from.
func WriteList(path string, files []string) error {
	var buf bytes.Buffer
	for _, f := range files {
		buf.WriteString(f)
		buf.WriteByte(0)
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}

// TempDir creates a private temporary directory in $XDG_RUNTIME_DIR (fallback
// os.TempDir()).
func TempDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	return os.MkdirTemp(base, "stick-backup-")
}

// ListTree returns all non-directory entries below dir (relative), used to
// compare the mirror with a file list.
func ListTree(dir string) (map[string]fs.FileInfo, error) {
	out := map[string]fs.FileInfo{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(dir, p)
		out[r] = fi
		return nil
	})
	return out, err
}
