package gitfilter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "protocol.file.allow=always", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(env(), "HOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, opt Options) *Result {
	t.Helper()
	opt.TmpDir = t.TempDir()
	r, err := BuildList(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func has(r *Result, f string) bool { return slices.Contains(r.Files, f) }

func TestBuildList(t *testing.T) {
	if _, err := Version(context.Background()); err != nil {
		t.Skip(err)
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	// Plain directory (no repo) with a .gitignore.
	write(t, root, ".gitignore", "node_modules/\n")
	write(t, root, "plain/keep.txt", "k")
	write(t, root, "plain/.env", "e")
	write(t, root, "plain/node_modules/m.js", "m")
	write(t, root, "plain/node_modules/.env", "e")
	write(t, root, "big.iso", "iso")
	write(t, root, "sp ace.txt", "s")
	write(t, root, "new\nline.txt", "n")
	os.Symlink("plain/keep.txt", filepath.Join(root, "link"))
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)

	// Repo with negation, nested .gitignore, tracked-but-deleted file.
	repo := filepath.Join(root, "repoA")
	write(t, repo, ".gitignore", "*.log\n!keep.log\n")
	write(t, repo, "x.log", "x")
	write(t, repo, "keep.log", "k")
	write(t, repo, "code.go", "c")
	write(t, repo, "gone.go", "g")
	write(t, repo, "sub/.gitignore", "*.gen\n")
	write(t, repo, "sub/a.gen", "a")
	write(t, repo, "sub/a.src", "a")
	write(t, repo, "node_modules/n.js", "n") // root .gitignore does NOT apply inside repoA
	run(t, repo, "init", "-q")
	run(t, repo, "add", ".gitignore", "code.go", "gone.go", "sub/.gitignore")
	run(t, repo, "commit", "-qm", "i")
	os.Remove(filepath.Join(repo, "gone.go"))

	// Untracked nested repo without own .gitignore and one with.
	write(t, repo, "nested/s.txt", "s")
	write(t, repo, "nested/a.dat", "d")
	run(t, filepath.Join(repo, "nested"), "init", "-q")
	write(t, root, "other/.gitignore", "*.dat\n")
	write(t, root, "other/a.dat", "d")
	write(t, root, "other/b.txt", "b")
	run(t, filepath.Join(root, "other"), "init", "-q")

	// Submodule (.git is a file).
	sm := filepath.Join(base, "sm")
	write(t, sm, "s.txt", "s")
	run(t, sm, "init", "-q")
	run(t, sm, "add", ".")
	run(t, sm, "commit", "-qm", "s")
	run(t, repo, "submodule", "add", "-q", sm, "subm")
	run(t, repo, "commit", "-qm", "sub")
	write(t, repo, "subm/untracked.txt", "u")

	// A global excludes file of the user must be ignored.
	write(t, base, "home/.config/git/ignore", "keep.txt\nb.txt\n")
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("XDG_CONFIG_HOME", "")

	r := build(t, Options{Root: root, Excludes: []string{"*.iso"}, Includes: []string{"**/.env"}, GitDirs: true})
	for _, f := range []string{
		".gitignore", "plain/keep.txt", "plain/.env", "plain/node_modules/.env", "sp ace.txt", "new\nline.txt", "link",
		"repoA/.gitignore", "repoA/keep.log", "repoA/code.go", "repoA/sub/.gitignore", "repoA/sub/a.src", "repoA/node_modules/n.js",
		"repoA/nested/s.txt", "repoA/nested/a.dat", "other/b.txt", "other/.gitignore",
		"repoA/subm/s.txt", "repoA/subm/untracked.txt", "repoA/subm/.git", "repoA/.git/HEAD", "repoA/nested/.git/HEAD",
		"repoA/.git/modules/subm/HEAD",
	} {
		if !has(r, f) {
			t.Errorf("missing %q", f)
		}
	}
	for _, f := range []string{"big.iso", "plain/node_modules/m.js", "repoA/x.log", "repoA/sub/a.gen", "repoA/gone.go", "other/a.dat", "repoA/subm", "repoA/nested", "empty"} {
		if has(r, f) {
			t.Errorf("unexpected %q", f)
		}
	}
	if !slices.IsSorted(r.Files) {
		t.Error("not sorted")
	}
	if len(r.Warnings) > 0 {
		t.Errorf("warnings: %v", r.Warnings)
	}

	// backup_git_dirs = false, no includes.
	r = build(t, Options{Root: root, GitDirs: false})
	for _, f := range r.Files {
		if strings.Contains(f, ".git/") || strings.HasSuffix(f, "/.git") {
			t.Errorf("git dir file %q present", f)
		}
	}
	if has(r, "plain/.env") != true || has(r, "plain/node_modules/.env") {
		t.Error("include handling without includes wrong")
	}

	// Mapping root is itself a repository.
	r = build(t, Options{Root: repo, GitDirs: false})
	if !has(r, "code.go") || has(r, "x.log") || !has(r, "nested/s.txt") || !has(r, "subm/s.txt") {
		t.Errorf("root repo: %v", r.Files)
	}
	// Source must stay untouched: no .git created in the plain root.
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		t.Error(".git created in source")
	}
}
