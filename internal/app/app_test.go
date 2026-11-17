package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/schubsijan/stick-backup/internal/config"
	"github.com/schubsijan/stick-backup/internal/medium"
	"github.com/schubsijan/stick-backup/internal/rsync"
	"github.com/schubsijan/stick-backup/internal/state"
	"github.com/schubsijan/stick-backup/internal/ui"
)

type env struct {
	t     *testing.T
	med   string
	home  string
	clock time.Time
	out   *bytes.Buffer
}

func newEnv(t *testing.T, mappings string) *env {
	t.Helper()
	if _, _, err := rsync.Version(context.Background()); err != nil {
		t.Skip("rsync missing")
	}
	dir := t.TempDir()
	e := &env{t: t, med: filepath.Join(dir, "med"), home: filepath.Join(dir, "home"),
		clock: time.Date(2026, 11, 1, 12, 0, 0, 0, time.Local), out: &bytes.Buffer{}}
	os.MkdirAll(e.home, 0o755)
	os.MkdirAll(e.med, 0o755)
	if err := e.app("h1", "", true).Init(context.Background(), e.med); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(filepath.Join(e.med, config.FileName), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(mappings)
	f.Close()
	return e
}

func (e *env) app(host, input string, yes bool) *App {
	e.out.Reset()
	return &App{
		UI: ui.NewTest(strings.NewReader(input), e.out, yes), Opt: Options{Target: e.med, Yes: yes},
		Host: host, User: "u", Home: e.home,
		Now: func() time.Time { return e.clock },
		Mounts: func() ([]medium.Mount, error) {
			return []medium.Mount{{MountPoint: e.med, FSType: "exfat", Source: "/dev/test"}}, nil
		},
	}
}

func (e *env) write(rel, content string) {
	p := filepath.Join(e.home, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// run executes `backup run` as host with --yes and advances the clock.
func (e *env) run(host string, opt Options) error {
	e.t.Helper()
	a := e.app(host, "", true)
	opt.Target, opt.Yes = e.med, true
	a.Opt = opt
	err := a.Run(context.Background())
	e.clock = e.clock.Add(time.Hour)
	return err
}

func (e *env) mirror(rel string) string { return filepath.Join(e.med, "mirror", rel) }

func (e *env) state() *state.State {
	st, err := state.Read(e.med)
	if err != nil || st == nil {
		e.t.Fatalf("state: %v", err)
	}
	return st
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func okOrWarn(t *testing.T, err error, out *bytes.Buffer) {
	t.Helper()
	if err != nil && Code(err) != ExitWarnings {
		t.Fatalf("%v\n%s", err, out)
	}
}

const twoMappings = `
[[mapping]]
name = "Docs"
source = "~/docs/"
target = "Dokumente/docs/"
exclude = ["**/node_modules/"]

[[mapping]]
name = "Saves"
source = "~/saves"
target = "Spiele/saves"
use_default_excludes = false
`

func TestMirror(t *testing.T) {
	e := newEnv(t, twoMappings)
	if !exists(filepath.Join(e.med, "mirror")) || !exists(filepath.Join(e.med, "logs")) || exists(filepath.Join(e.med, "snapshots")) {
		t.Fatal("init layout wrong")
	}
	e.write("docs/a", "a")
	e.write("docs/b", "b")
	e.write("docs/c", "c")
	e.write("docs/sub/d", "d")
	e.write("docs/x.tmp", "excluded by defaults")
	e.write("docs/p/node_modules/m.js", "excluded")
	e.write("saves/w/r.mca", "region")
	e.write("saves/w/s.tmp", "kept: no default excludes")
	os.Symlink("a", filepath.Join(e.home, "docs/link"))

	if err := e.run("h1", Options{}); err != nil {
		t.Fatalf("run1: %v\n%s", err, e.out)
	}
	if !strings.Contains(e.out.String(), "1 Symlinks übersprungen") {
		t.Errorf("no symlink hint:\n%s", e.out)
	}
	for _, f := range []string{"Dokumente/docs/a", "Dokumente/docs/sub/d", "Spiele/saves/w/r.mca", "Spiele/saves/w/s.tmp"} {
		if !exists(e.mirror(f)) {
			t.Errorf("missing %s", f)
		}
	}
	for _, f := range []string{"Dokumente/docs/x.tmp", "Dokumente/docs/p/node_modules", "Dokumente/docs/link"} {
		if exists(e.mirror(f)) {
			t.Errorf("unexpected %s", f)
		}
	}
	st := e.state()
	if st.Status != state.StatusComplete || st.Host != "h1" || len(st.Mappings) != 2 || st.Mappings[0].Host != "h1" || st.Mappings[0].FilesTotal != 4 {
		t.Fatalf("state: %+v", st)
	}
	if logs, _ := os.ReadDir(filepath.Join(e.med, "logs")); len(logs) != 1 {
		t.Errorf("logs: %v", logs)
	}
	if exists(filepath.Join(e.med, ".backup.lock")) {
		t.Error("lock left behind")
	}

	// Changed and deleted file (1 of 4 = 25 % > 20 % → warning, --yes confirms).
	e.write("docs/a", "A2")
	os.Remove(filepath.Join(e.home, "docs/b"))
	if err := e.run("h1", Options{}); Code(err) != ExitWarnings {
		t.Fatalf("run2: %v\n%s", err, e.out)
	}
	if b, _ := os.ReadFile(e.mirror("Dokumente/docs/a")); string(b) != "A2" {
		t.Errorf("change not mirrored: %q", b)
	}
	if exists(e.mirror("Dokumente/docs/b")) {
		t.Error("deleted file still in mirror")
	}
	if e.state().Mappings[0].FilesDeleted != 1 {
		t.Errorf("FilesDeleted: %+v", e.state().Mappings[0])
	}

	// A newly excluded file disappears from the mirror (--delete-excluded).
	os.WriteFile(e.mirror("Dokumente/docs/x.tmp"), []byte("stale"), 0o644)
	okOrWarn(t, e.run("h1", Options{}), e.out)
	if exists(e.mirror("Dokumente/docs/x.tmp")) {
		t.Error("excluded file not removed")
	}

	// --only keeps the state of the other mapping.
	if err := e.run("h1", Options{Only: []string{"saves"}}); err != nil {
		t.Fatalf("only: %v\n%s", err, e.out)
	}
	st = e.state()
	if len(st.Mappings) != 2 || st.Mappings[0].Name != "Saves" || st.Mappings[1].Name != "Docs" || st.Mappings[1].UpdatedAt == nil {
		t.Errorf("only state: %+v", st.Mappings)
	}

	// status and verify work.
	if err := e.app("h1", "", true).Status(context.Background()); err != nil {
		t.Errorf("status: %v\n%s", err, e.out)
	}
	if err := e.app("h1", "", true).Verify(context.Background()); err != nil {
		t.Errorf("verify: %v\n%s", err, e.out)
	}
	// Same size and mtime, different content → reported by verify.
	p := e.mirror("Spiele/saves/w/r.mca")
	fi, _ := os.Stat(p)
	os.WriteFile(p, []byte("REGION"), 0o644)
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	if err := e.app("h1", "", true).Verify(context.Background()); Code(err) != ExitWarnings || !strings.Contains(e.out.String(), "w/r.mca") {
		t.Errorf("bitrot not found: %v\n%s", err, e.out)
	}
}

func TestMassDeleteAndDryRun(t *testing.T) {
	e := newEnv(t, twoMappings)
	for _, f := range []string{"a", "b", "c", "d", "e"} {
		e.write("docs/"+f, f)
	}
	e.write("saves/x", "x")
	if err := e.run("h1", Options{}); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(e.home, "docs/a"))
	os.Remove(filepath.Join(e.home, "docs/b"))
	// 2 of 5 = 40 %: answer "n" → nothing changes.
	a := e.app("h1", "n\n", false)
	if err := a.Run(context.Background()); Code(err) != ExitAborted || !strings.Contains(e.out.String(), "2 von 5 Dateien") {
		t.Fatalf("expected abort: %v\n%s", err, e.out)
	}
	if !exists(e.mirror("Dokumente/docs/a")) {
		t.Error("mirror changed despite no")
	}
	// Dry run writes nothing (no log, no lock, no state change).
	logs, _ := os.ReadDir(filepath.Join(e.med, "logs"))
	before := e.state().StartedAt
	a = e.app("h1", "", false)
	a.Opt.DryRun = true
	err := a.Run(context.Background())
	okOrWarn(t, err, e.out)
	if !strings.Contains(e.out.String(), "2 würden im Spiegel gelöscht") {
		t.Errorf("dry-run summary:\n%s", e.out)
	}
	logs2, _ := os.ReadDir(filepath.Join(e.med, "logs"))
	if len(logs2) != len(logs) || !e.state().StartedAt.Equal(before) || !exists(e.mirror("Dokumente/docs/a")) {
		t.Error("dry-run wrote to the medium")
	}
	// "j" applies it.
	a = e.app("h1", "j\n", false)
	if err := a.Run(context.Background()); Code(err) != ExitWarnings {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if exists(e.mirror("Dokumente/docs/a")) {
		t.Error("deletion not applied")
	}
}

func TestOtherHostNewer(t *testing.T) {
	e := newEnv(t, twoMappings)
	e.write("docs/a", "a")
	e.write("saves/x", "x")
	if err := e.run("h1", Options{}); err != nil {
		t.Fatal(err)
	}
	// h2 writes a newer x.
	e.write("saves/x", "x2")
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(e.home, "saves/x"), future, future)
	if err := e.run("h2", Options{}); err != nil {
		t.Fatalf("h2: %v\n%s", err, e.out)
	}
	if e.state().Mapping("Spiele/saves").Host != "h2" {
		t.Fatal("state host not h2")
	}
	// Back on h1 with the old x: warning + question.
	e.write("saves/x", "x")
	a := e.app("h1", "n\n", false)
	if err := a.Run(context.Background()); Code(err) != ExitAborted || !strings.Contains(e.out.String(), "Saves wurde zuletzt von h2 gesichert") {
		t.Fatalf("expected warning+abort: %v\n%s", err, e.out)
	}
	if b, _ := os.ReadFile(e.mirror("Spiele/saves/x")); string(b) != "x2" {
		t.Error("mirror overwritten despite no")
	}
}

func TestEmptyAndMissingSource(t *testing.T) {
	e := newEnv(t, twoMappings)
	e.write("docs/a", "a")
	os.MkdirAll(filepath.Join(e.home, "saves"), 0o755)
	if err := e.run("h1", Options{}); err == nil || !strings.Contains(err.Error(), "leer") {
		t.Fatalf("empty source: %v", err)
	}
	os.RemoveAll(filepath.Join(e.home, "saves"))
	if err := e.run("h1", Options{}); err == nil || !strings.Contains(err.Error(), "fehlt") {
		t.Fatalf("missing source: %v", err)
	}
	if exists(filepath.Join(e.med, state.FileName)) {
		t.Error("state written")
	}
}

func TestAbort(t *testing.T) {
	e := newEnv(t, twoMappings)
	e.write("docs/a", "a")
	e.write("saves/x", "x")
	// Wrapper that blocks on the real copy (the one with progress output).
	wrapper := filepath.Join(t.TempDir(), "rsync")
	real, _ := exec.LookPath("rsync")
	os.WriteFile(wrapper, []byte("#!/bin/sh\ncase \"$*\" in *progress2*) exec sleep 30;; esac\nexec "+real+" \"$@\"\n"), 0o755)
	old := rsync.Binary
	rsync.Binary = wrapper
	defer func() { rsync.Binary = old }()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(700*time.Millisecond, cancel)
	if err := e.app("h1", "", true).Run(ctx); Code(err) != ExitAborted {
		t.Fatalf("expected abort: %v\n%s", err, e.out)
	}
	st := e.state()
	if st.Status != state.StatusAborted || st.Mappings[0].Status != state.MappingFailed || st.Mappings[1].Status != state.MappingSkipped {
		t.Fatalf("state: %+v", st)
	}
	if exists(filepath.Join(e.med, ".backup.lock")) {
		t.Error("lock left behind")
	}
	rsync.Binary = old
	if err := e.run("h1", Options{}); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if e.state().Status != state.StatusComplete {
		t.Error("not repaired by next run")
	}
}

func TestCaseCollision(t *testing.T) {
	e := newEnv(t, twoMappings)
	e.write("docs/Readme", "1")
	e.write("docs/README", "2")
	e.write("docs/Sub/a", "a")
	e.write("docs/sub/b", "b")
	e.write("saves/x", "x")
	err := e.run("h1", Options{})
	if Code(err) != ExitWarnings || !strings.Contains(e.out.String(), "2 Namen unterscheiden sich nur in Groß-/Kleinschreibung") {
		t.Fatalf("%v\n%s", err, e.out)
	}
	got := caseCollisions([]string{"a/B", "A/b", "c", "C", "d"})
	if !reflect.DeepEqual(got, [][]string{{"A/b", "a/B"}, {"C", "c"}}) {
		t.Errorf("got %v", got)
	}
	if p := withParents([]string{"x/y/z", "x/w"}); strings.Join(p, ",") != "x/y/z,x/y,x,x/w" {
		t.Errorf("parents %v", p)
	}
}

func TestGitFilter(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git missing")
	}
	e := newEnv(t, `
[[mapping]]
name = "Src"
source = "~/src"
target = "src"
filter = "git"
include = ["**/.env"]
backup_git_dirs = false
`)
	repo := filepath.Join(e.home, "src/p")
	e.write("src/p/.gitignore", "*.o\n.env\n")
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		e.write("src/p/dir/"+f, f)
	}
	e.write("src/p/x.o", "obj")
	e.write("src/p/.env", "SECRET=1")
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if err := e.run("h1", Options{}); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	m := e.mirror("src/p")
	if !exists(filepath.Join(m, "dir/a.go")) || !exists(filepath.Join(m, ".env")) || exists(filepath.Join(m, "x.o")) || exists(filepath.Join(m, ".git")) {
		t.Fatal("first mirror wrong")
	}
	os.Remove(filepath.Join(repo, "dir/a.go"))
	okOrWarn(t, e.run("h1", Options{}), e.out)
	if exists(filepath.Join(m, "dir/a.go")) || !exists(filepath.Join(m, "dir/b.go")) {
		t.Error("unlisted file not removed / listed file lost")
	}
	os.RemoveAll(filepath.Join(repo, "dir"))
	okOrWarn(t, e.run("h1", Options{}), e.out)
	if exists(filepath.Join(m, "dir")) {
		t.Error("empty dir left")
	}
	if err := e.app("h1", "", true).Verify(context.Background()); err != nil {
		t.Errorf("verify: %v\n%s", err, e.out)
	}
}

func TestLocks(t *testing.T) {
	e := newEnv(t, twoMappings)
	e.write("docs/a", "a")
	e.write("saves/x", "x")
	os.WriteFile(filepath.Join(e.med, ".backup.lock"), []byte(`{"host":"h1","pid":4194304,"command":"run"}`), 0o644)
	if err := e.app("h1", "n\n", false).Run(context.Background()); Code(err) != ExitLocked {
		t.Fatalf("declined stale lock: %v", err)
	}
	os.WriteFile(filepath.Join(e.med, ".backup.lock"), []byte(`{"host":"other","pid":1,"command":"run"}`), 0o644)
	if err := e.run("h1", Options{}); Code(err) != ExitLocked {
		t.Fatalf("foreign lock: %v", err)
	}
	if err := e.run("h1", Options{ForceUnlock: true}); err != nil {
		t.Fatalf("force-unlock: %v\n%s", err, e.out)
	}
}

func TestStderrPaths(t *testing.T) {
	got := stderrPaths([]string{
		`rsync: [receiver] mkstemp "/m/h/Docs/sub/.co:lon.73Fjbo" failed: Invalid argument (22)`,
		`rsync: [sender] send_files failed to open "/src/locked/secret": Permission denied (13)`,
		`rsync error: some files/attrs were not transferred (code 23)`,
	}, "/src", "/m/h/Docs")
	if !reflect.DeepEqual(got, map[string]bool{"sub/co:lon": true, "locked/secret": true}) {
		t.Errorf("got %v", got)
	}
}

func TestStaticHostname(t *testing.T) {
	for in, want := range map[string]string{"laptop\n": "laptop", "\n": "", "": "", "# c\n\n  desk.local \n": "desk.local"} {
		if got := ParseHostnameFile([]byte(in)); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	dir := t.TempDir()
	empty := filepath.Join(dir, "hostname")
	os.WriteFile(empty, []byte("\n"), 0o644)
	if _, err := StaticHostname(empty); err == nil || !strings.Contains(err.Error(), "hostnamectl") {
		t.Errorf("empty file: %v", err)
	}
	a := &App{HostErr: errors.New("no host"), UI: ui.NewTest(strings.NewReader(""), &bytes.Buffer{}, true)}
	if err := a.Run(context.Background()); err == nil || err.Error() != "no host" {
		t.Errorf("run without host: %v", err)
	}
}

func TestInitChoosesDrive(t *testing.T) {
	dir := t.TempDir()
	stick := filepath.Join(dir, "stick")
	done := filepath.Join(dir, "done")
	os.Mkdir(stick, 0o755)
	os.Mkdir(done, 0o755)
	os.WriteFile(filepath.Join(done, config.FileName), []byte("x"), 0o644)
	out := &bytes.Buffer{}
	a := &App{UI: ui.NewTest(strings.NewReader("1\nStick\n"), out, false), User: "u", HostErr: errors.New("kein fester Hostname"),
		Mounts: func() ([]medium.Mount, error) { return []medium.Mount{{MountPoint: stick, FSType: "exfat"}}, nil },
		Drives: func(context.Context) []medium.Drive {
			return []medium.Drive{
				{Device: "/dev/sdc1", MountPoint: done, FSType: "exfat"},
				{Device: "/dev/sdb1", MountPoint: stick, FSType: "exfat", Label: "INTENSO", Size: 62e9},
			}
		}}
	if err := a.Init(context.Background(), ""); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	o := out.String()
	if !strings.Contains(o, "[1] /dev/sdb1  INTENSO (exfat, 62,0 GB) – "+stick) || !strings.Contains(o, "[2] aktuelles Verzeichnis") || strings.Contains(o, "sdc1") || !strings.Contains(o, "kein fester Hostname") {
		t.Errorf("listing:\n%s", o)
	}
	c, err := config.Load(filepath.Join(stick, config.FileName))
	if err != nil || c.Medium.Name != "Stick" || !exists(filepath.Join(stick, "mirror")) {
		t.Fatalf("config: %v %+v", err, c)
	}
}

func TestCodes(t *testing.T) {
	if Code(ui.ErrAborted) != ExitAborted || Code(context.Canceled) != ExitAborted || Code(errors.New("x")) != ExitError || Code(&ExitErr{Code: 4}) != 4 {
		t.Error("codes")
	}
}

// fakeStick simulates a plugged-in but unmounted stick that MountDrive mounts.
type fakeStick struct {
	dir     string
	mounted bool
}

func (f *fakeStick) wire(a *App) {
	a.Mounts = func() ([]medium.Mount, error) {
		if f.mounted {
			return []medium.Mount{{MountPoint: f.dir, FSType: "exfat", Source: "/dev/sda1"}}, nil
		}
		return nil, nil
	}
	a.Drives = func(context.Context) []medium.Drive {
		d := medium.Drive{Device: "/dev/sda1", FSType: "exfat", Label: "INTENSO", Size: 62e9}
		if f.mounted {
			d.MountPoint = f.dir
		}
		return []medium.Drive{d}
	}
	a.MountDrive = func(_ context.Context, dev string) (string, error) {
		if dev != "/dev/sda1" {
			return "", errors.New("wrong device")
		}
		f.mounted = true
		return f.dir, nil
	}
}

func TestUnmountedStick(t *testing.T) {
	stick := &fakeStick{dir: t.TempDir()}
	out := &bytes.Buffer{}
	// init: the unmounted stick is offered and mounted when chosen.
	a := &App{UI: ui.NewTest(strings.NewReader("1\nINTENSO\n"), out, false), User: "u", Host: "h1"}
	stick.wire(a)
	if err := a.Init(context.Background(), ""); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "[1] /dev/sda1  INTENSO (exfat, 62,0 GB) – nicht eingehängt") || !exists(filepath.Join(stick.dir, config.FileName)) {
		t.Fatalf("init:\n%s", out)
	}
	// Later the stick is unplugged and plugged in again (not mounted):
	// status offers to mount it and then finds the medium.
	stick.mounted = false
	out.Reset()
	a = &App{UI: ui.NewTest(strings.NewReader("j\nj\n"), out, false), User: "u", Host: "h1"}
	stick.wire(a)
	if err := a.Status(context.Background()); err != nil && Code(err) != ExitWarnings {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "Einhängen und nach backup.toml suchen?") || !strings.Contains(out.String(), "Backup-Medium gefunden") {
		t.Errorf("status:\n%s", out)
	}
	// Declining the mount → no medium.
	stick.mounted = false
	out.Reset()
	a = &App{UI: ui.NewTest(strings.NewReader("n\n"), out, false), User: "u", Host: "h1"}
	stick.wire(a)
	if err := a.Status(context.Background()); Code(err) != ExitNoMedium {
		t.Errorf("declined: %v\n%s", err, out)
	}
}

func TestIncludeWhitelist(t *testing.T) {
	e := newEnv(t, `
[[mapping]]
name   = "Minecraft"
source = "~/inst/"
target = "Minecraft/"
include = ["instance.cfg", "minecraft/options.txt", "minecraft/saves/Chill/", "minecraft/saves/Farms 2/"]
`)
	for _, f := range []string{"instance.cfg", "mmc-pack.json", "minecraft/options.txt", "minecraft/x.tmp",
		"minecraft/saves/Chill/level.dat", "minecraft/saves/Chill/a.tmp", "minecraft/saves/Farms 2/region/r.mca",
		"minecraft/saves/Other/level.dat", "minecraft/logs/l.log"} {
		e.write("inst/"+f, f)
	}
	if err := e.run("h1", Options{}); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	for _, f := range []string{"instance.cfg", "minecraft/options.txt", "minecraft/saves/Chill/level.dat", "minecraft/saves/Farms 2/region/r.mca"} {
		if !exists(e.mirror("Minecraft/" + f)) {
			t.Errorf("missing %s", f)
		}
	}
	for _, f := range []string{"mmc-pack.json", "minecraft/x.tmp", "minecraft/saves/Chill/a.tmp", "minecraft/saves/Other", "minecraft/logs"} {
		if exists(e.mirror("Minecraft/" + f)) {
			t.Errorf("unexpected %s", f)
		}
	}
	// A new cache folder is not picked up.
	e.write("inst/minecraft/newcache/junk", "j")
	okOrWarn(t, e.run("h1", Options{}), e.out)
	if exists(e.mirror("Minecraft/minecraft/newcache")) {
		t.Error("new folder mirrored despite whitelist")
	}
	// Removing a path from the list removes it from the mirror; a typo warns.
	p := filepath.Join(e.med, config.FileName)
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), `"minecraft/options.txt", `, `"minecraft/optoins.txt", `, 1)), 0o644)
	err := e.run("h1", Options{})
	if Code(err) != ExitWarnings || !strings.Contains(e.out.String(), "include-Pfade gibt es in der Quelle nicht") || !strings.Contains(e.out.String(), "optoins.txt") {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if exists(e.mirror("Minecraft/minecraft/options.txt")) {
		t.Error("removed include still in mirror")
	}
	if err := e.app("h1", "", true).Verify(context.Background()); err != nil {
		t.Errorf("verify: %v\n%s", err, e.out)
	}
}

func TestEjectAfterRun(t *testing.T) {
	e := newEnv(t, twoMappings)
	e.write("docs/a", "a")
	e.write("saves/x", "x")
	var unmounted []string
	wire := func(a *App, removable bool) {
		a.Drives = func(context.Context) []medium.Drive {
			if !removable {
				return nil
			}
			return []medium.Drive{{Device: "/dev/test", MountPoint: e.med, FSType: "exfat"}}
		}
		a.UnmountDrive = func(_ context.Context, dev string) error {
			unmounted = append(unmounted, dev)
			return nil
		}
		synced := 0
		a.Sync = func() { synced++ }
	}
	// Enter = yes → ejected.
	a := e.app("h1", "\n", false)
	wire(a, true)
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if len(unmounted) != 1 || unmounted[0] != "/dev/test" || !strings.Contains(e.out.String(), "kannst ihn jetzt abziehen") {
		t.Fatalf("not ejected: %v\n%s", unmounted, e.out)
	}
	if exists(filepath.Join(e.med, ".backup.lock")) {
		t.Error("lock still there at eject time")
	}
	// "n" → not ejected, hint shown.
	a = e.app("h1", "n\n", false)
	wire(a, true)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(unmounted) != 1 || !strings.Contains(e.out.String(), "Vor dem Abziehen auswerfen") {
		t.Errorf("ejected despite no: %v\n%s", unmounted, e.out)
	}
	// Not a removable drive → no question.
	a = e.app("h1", "", false)
	wire(a, false)
	if err := a.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.out.String(), "auswerfen?") {
		t.Errorf("eject offered for non-removable:\n%s", e.out)
	}
}

func TestChecksumRepairs(t *testing.T) {
	e := newEnv(t, twoMappings)
	e.write("docs/a", "aaaaaaaaaa")
	e.write("saves/x", "x")
	if err := e.run("h1", Options{}); err != nil {
		t.Fatal(err)
	}
	// Damage the mirror copy: same size and mtime, other content.
	p := e.mirror("Dokumente/docs/a")
	fi, _ := os.Stat(p)
	os.WriteFile(p, []byte("aaaaaXXXXX"), 0o644)
	os.Chtimes(p, fi.ModTime(), fi.ModTime())
	if err := e.app("h1", "", true).Verify(context.Background()); Code(err) != ExitWarnings || !strings.Contains(e.out.String(), "stick-backup run --checksum") {
		t.Fatalf("verify: %v\n%s", err, e.out)
	}
	okOrWarn(t, e.run("h1", Options{}), e.out)
	if b, _ := os.ReadFile(p); string(b) != "aaaaaXXXXX" {
		t.Fatal("normal run should not notice (size+mtime equal)")
	}
	if err := e.run("h1", Options{Checksum: true}); err != nil {
		t.Fatalf("%v\n%s", err, e.out)
	}
	if b, _ := os.ReadFile(p); string(b) != "aaaaaaaaaa" {
		t.Errorf("not repaired: %q", b)
	}
	if err := e.app("h1", "", true).Verify(context.Background()); err != nil {
		t.Errorf("verify after repair: %v\n%s", err, e.out)
	}
}
