package rsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseItem(t *testing.T) {
	cases := []struct {
		line           string
		path, link     string
		del, dir, xfer bool
	}{
		{">f+++++++++ a.txt", "a.txt", "", false, false, true},
		{">f.st...... sub/b c.txt", "sub/b c.txt", "", false, false, true},
		{"cd+++++++++ sub/deep/", "sub/deep/", "", false, true, false},
		{".d..t...... ./", "./", "", false, true, false},
		{"*deleting   sub/deep/c.txt", "sub/deep/c.txt", "", true, false, false},
		{"*deleting   sub/deep/", "sub/deep/", "", true, true, false},
		{"cL+++++++++ link -> b.txt", "link", "b.txt", false, false, false},
		{"hf+++++++++ x => y", "x", "y", false, false, false},
		{`>f+++++++++ new\#012line.txt`, "new\nline.txt", "", false, false, true},
	}
	for _, c := range cases {
		it, ok := ParseItem(c.line)
		if !ok {
			t.Errorf("%q not parsed", c.line)
			continue
		}
		if it.Path != c.path || it.Link != c.link || it.IsDelete() != c.del || it.IsDir() != c.dir || it.IsTransfer() != c.xfer {
			t.Errorf("%q: %+v del=%v dir=%v xfer=%v", c.line, it, it.IsDelete(), it.IsDir(), it.IsTransfer())
		}
	}
	for _, l := range []string{"created directory snap/s1", "sent 1 bytes  received 2 bytes", "", "Number of files: 7", "rsync error: x"} {
		if _, ok := ParseItem(l); ok {
			t.Errorf("%q must not parse", l)
		}
	}
	if it, _ := ParseItem(">fc........ a"); !it.ContentOnlyDiff() {
		t.Error("content-only diff not detected")
	}
	if it, _ := ParseItem(">fcst...... a"); it.ContentOnlyDiff() {
		t.Error("normal change detected as content-only")
	}
	if it, _ := ParseItem(".d..t...... x/"); !it.IsDirTimeOnly() {
		t.Error("dir time only")
	}
}

func TestParseStats(t *testing.T) {
	for _, out := range []string{
		"Number of files: 1.234 (reg: 1.000, dir: 200, link: 34)\nNumber of regular files transferred: 7\nTotal file size: 60.000.008 bytes\nTotal transferred file size: 150.000 bytes\n",
		"Number of files: 1,234 (reg: 1,000, dir: 200, link: 34)\nNumber of regular files transferred: 7\nTotal file size: 60,000,008 bytes\nTotal transferred file size: 150,000 bytes\n",
		"Number of files: 1234 (reg: 1000, dir: 200, link: 34)\nNumber of created files: 3\nNumber of regular files transferred: 7\nTotal file size: 60000008 bytes\nTotal transferred file size: 150000 bytes\n",
	} {
		s := ParseStats(out)
		if !s.Valid || s.Files != 1234 || s.RegFiles != 1000 || s.Dirs != 200 || s.NonDirs() != 1034 || s.FilesTransferred != 7 || s.TotalSize != 60000008 || s.TransferredSize != 150000 {
			t.Errorf("%+v", s)
		}
	}
	if s := ParseStats("Number of files: 0\nTotal transferred file size: 0 bytes"); !s.Valid || s.Files != 0 {
		t.Errorf("%+v", s)
	}
}

func TestVersion(t *testing.T) {
	v, s, ok := ParseVersion("rsync  version 3.5.2-ga8306e06  protocol version 33\nCopyright")
	if !ok || v != [3]int{3, 5, 2} || s != "3.5.2-ga8306e06" {
		t.Errorf("%v %q", v, s)
	}
	v, _, ok = ParseVersion("rsync  version v3.1.0  protocol version 31")
	if !ok || !AtLeast(v, MinVersion) {
		t.Errorf("3.1.0: %v", v)
	}
	v, _, _ = ParseVersion("rsync  version 3.0.9  protocol version 30")
	if AtLeast(v, MinVersion) {
		t.Error("3.0.9 must be too old")
	}
	if v, _, ok := ParseVersion("git version 2.56.0"); !ok || v != [3]int{2, 56, 0} {
		t.Errorf("git: %v", v)
	}
}

func TestArgs(t *testing.T) {
	a := Args(Options{Source: "/s", Dest: "/d/", Excludes: []string{"*.tmp"}, DryRun: true, Stats: true, Delete: true, DeleteExcluded: true})
	got := strings.Join(a, " ")
	want := "-rt -n --modify-window=1 --delete --delete-excluded --exclude=*.tmp --itemize-changes --out-format=%i %n%L --stats --no-human-readable /s/ /d/"
	if got != want {
		t.Errorf("\ngot  %s\nwant %s", got, want)
	}
	if p, ok := SkippedNonRegular(`skipping non-regular file "a b/link"`); !ok || p != "a b/link" {
		t.Errorf("skip: %q %v", p, ok)
	}
}

// TestRunReal runs the installed rsync once to check output handling.
func TestRunReal(t *testing.T) {
	if _, _, err := Version(context.Background()); err != nil {
		t.Skip("rsync missing")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "a"), make([]byte, 5000), 0o644)
	os.WriteFile(filepath.Join(src, "sub", "b"), []byte("b"), 0o644)
	os.WriteFile(filepath.Join(src, "x.tmp"), []byte("x"), 0o644)
	var items []Item
	var progress int
	res, err := Run(context.Background(), Args(Options{Source: src, Dest: filepath.Join(dir, "dst"), Excludes: []string{"*.tmp"}, Stats: true, Progress: true}), Handlers{
		Item:     func(i Item) { items = append(items, i) },
		Progress: func(string) { progress++ },
	})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("%v %+v", err, res)
	}
	if len(items) != 4 || progress == 0 {
		t.Errorf("items=%v progress=%d", items, progress)
	}
	if res.Stats.RegFiles != 2 || res.Stats.FilesTransferred != 2 || res.Stats.TransferredSize != 5001 {
		t.Errorf("stats %+v", res.Stats)
	}
}

func TestListFiles(t *testing.T) {
	if _, _, err := Version(context.Background()); err != nil {
		t.Skip("rsync missing")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sub dir"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub dir", "a b.txt"), make([]byte, 2000), 0o644)
	os.WriteFile(filepath.Join(dir, "x.tmp"), nil, 0o644)
	os.Symlink("sub dir", filepath.Join(dir, "link"))
	got, err := ListFiles(context.Background(), dir, []string{"*.tmp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "link|sub dir|sub dir/a b.txt" {
		t.Errorf("got %q", got)
	}
}

func TestIncludeRules(t *testing.T) {
	got := IncludeRules([]string{"instance.cfg", "/minecraft/saves/Chill/", "minecraft/config", "minecraft/saves/Farms 2/", ""})
	want := []string{
		"/instance.cfg", "/instance.cfg/***",
		"/minecraft/", "/minecraft/saves/", "/minecraft/saves/Chill/***",
		"/minecraft/config", "/minecraft/config/***",
		"/minecraft/saves/Farms 2/***",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("\ngot  %q\nwant %q", got, want)
	}
	a := strings.Join(Args(Options{Source: "/s", Dest: "/d", Excludes: []string{"*.tmp"}, Includes: []string{"a/b"}}), " ")
	if !strings.Contains(a, "--exclude=*.tmp --include=/a/ --include=/a/b --include=/a/b/*** --exclude=* --itemize") {
		t.Errorf("order: %s", a)
	}
}

func TestListFilesIncludes(t *testing.T) {
	if _, _, err := Version(context.Background()); err != nil {
		t.Skip("rsync missing")
	}
	dir := t.TempDir()
	for _, f := range []string{"instance.cfg", "mmc-pack.json", "mc/options.txt", "mc/x.tmp", "mc/saves/Chill/level.dat", "mc/saves/Chill/r/r.mca", "mc/saves/Other/level.dat", "mc/newcache/junk"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	got, err := ListFiles(context.Background(), dir, []string{"*.tmp"}, []string{"instance.cfg", "mc/options.txt", "mc/saves/Chill/"})
	if err != nil {
		t.Fatal(err)
	}
	want := "instance.cfg|mc|mc/options.txt|mc/saves|mc/saves/Chill|mc/saves/Chill/level.dat|mc/saves/Chill/r|mc/saves/Chill/r/r.mca"
	if strings.Join(got, "|") != want {
		t.Errorf("got %q", got)
	}
}
