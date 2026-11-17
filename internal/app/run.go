package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/schubsijan/stick-backup/internal/config"
	"github.com/schubsijan/stick-backup/internal/gitfilter"
	"github.com/schubsijan/stick-backup/internal/logx"
	"github.com/schubsijan/stick-backup/internal/medium"
	"github.com/schubsijan/stick-backup/internal/rsync"
	"github.com/schubsijan/stick-backup/internal/state"
	"github.com/schubsijan/stick-backup/internal/ui"
)

// LogMaxAge is the age after which log files are removed.
const LogMaxAge = 90 * 24 * time.Hour

// job is one mapping of a run.
type job struct {
	m        *config.Mapping
	src      string // resolved source
	target   string // cleaned target
	dst      string // mirror/<target>
	excludes []string
	includes []string // rsync filter: whitelist (empty = everything)
	files    []string // git filter: file list
	listPath string   // git filter: NUL-separated list for rsync
	prev     *state.Mapping
	ms       *state.Mapping // state of this run
	sim      *rsync.Result
	simDel   int
}

// opts returns rsync options with the mapping's file selection.
func (j *job) opts(src, dst string) rsync.Options {
	o := rsync.Options{Source: src, Dest: dst}
	if j.m.IsGit() {
		o.FilesFrom = j.listPath
	} else {
		o.Excludes, o.Includes = j.excludes, j.includes
	}
	return o
}

// newJob prepares a job; with filter = "rsync", include is a whitelist.
func newJob(cfg *config.Config, m *config.Mapping, src, mirrorDir string) *job {
	t := m.CleanTarget()
	j := &job{m: m, src: src, target: t, dst: filepath.Join(mirrorDir, t), excludes: cfg.Excludes(m)}
	if !m.IsGit() {
		j.includes = m.Include
	}
	return j
}

// missingIncludes returns whitelist paths without wildcards that do not
// exist in the source (typos).
func (j *job) missingIncludes() []string {
	var out []string
	for _, p := range j.includes {
		if strings.ContainsAny(p, "*?[") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(j.src, strings.Trim(p, "/"))); err != nil {
			out = append(out, p)
		}
	}
	return out
}

// mirrorExists reports whether the mapping already has data in the mirror.
func (j *job) mirrorExists() bool {
	fi, err := os.Stat(j.dst)
	return err == nil && fi.IsDir()
}

// runCtx holds the state of one `run`.
type runCtx struct {
	a        *App
	med      *Medium
	cfg      *config.Config
	log      *logx.Log
	jobs     []*job
	old      *state.State // state before this run
	st       *state.State // state written by this run
	warnings []string     // run-level warnings before st exists
	count    int          // total number of warnings
	tmpDir   string
	dryRun   bool
	fsync    bool // rsync --fsync available
}

// warn reports a warning to terminal, log and state.
func (r *runCtx) warn(j *job, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	r.count++
	r.a.UI.Warn("%s", msg)
	r.log.Printf("WARNUNG: %s", msg)
	if j != nil && j.ms != nil && r.st != nil {
		j.ms.Warnings = append(j.ms.Warnings, msg)
		if j.ms.Status == state.MappingOK {
			j.ms.Status = state.MappingWarning
		}
		return
	}
	if r.st != nil {
		r.st.Warnings = append(r.st.Warnings, msg)
	} else {
		r.warnings = append(r.warnings, msg)
	}
}

// confirm asks and logs the answer.
func (r *runCtx) confirm(ctx context.Context, question string) error {
	ok, err := r.a.UI.Confirm(ctx, question)
	switch {
	case err != nil:
		r.log.Printf("Frage %q: abgebrochen", question)
		return err
	case !ok:
		r.log.Printf("Frage %q: abgelehnt", question)
		return ui.ErrAborted
	case r.a.UI.Yes:
		r.log.Printf("Frage %q: bestätigt durch --yes", question)
	default:
		r.log.Printf("Frage %q: bestätigt durch Benutzer", question)
	}
	return nil
}

// Run implements `stick-backup run`. After the run itself (lock released, log
// closed) everything is flushed once more, and a removable stick is offered
// to be ejected – pulling it without unmounting loses writes and leaves the
// exFAT volume marked dirty.
func (a *App) Run(ctx context.Context) error {
	a.runMedium = nil
	err := a.run(ctx)
	med := a.runMedium
	if med == nil || a.Opt.DryRun {
		return err
	}
	if werr := a.waitForWrites(ctx, "Schreibe letzte Änderungen auf den Stick"); werr != nil && err == nil {
		err = ui.ErrAborted
	}
	if err == nil || Code(err) == ExitWarnings {
		a.offerEject(ctx, med)
	}
	return err
}

// offerEject unmounts the medium after asking, but only if it is a mounted
// removable drive (never an internal disk or a --target directory).
func (a *App) offerEject(ctx context.Context, med *Medium) {
	if a.Drives == nil || a.UnmountDrive == nil || med.Device == "" {
		return
	}
	removable := false
	for _, d := range a.Drives(ctx) {
		if d.Device == med.Device && d.MountPoint == med.Path {
			removable = true
		}
	}
	if !removable {
		return
	}
	a.UI.Println("")
	ok, err := a.UI.ConfirmYes(ctx, "Stick jetzt auswerfen?")
	if err != nil || !ok {
		a.UI.Hint("Vor dem Abziehen auswerfen (Dateimanager oder: udisksctl unmount -b %s) – sonst gehen Daten verloren.", med.Device)
		return
	}
	if err := a.UnmountDrive(ctx, med.Device); err != nil {
		a.UI.Warn("%v", err)
		a.UI.Hint("Bitte im Dateimanager auswerfen, bevor du den Stick abziehst.")
		return
	}
	a.UI.Success("Stick ausgeworfen – du kannst ihn jetzt abziehen.")
}

func (a *App) run(ctx context.Context) (retErr error) {
	if err := a.requireHost(); err != nil {
		return err
	}
	start := a.Now()
	med, err := a.selectMedium(ctx)
	if err != nil {
		return err
	}
	a.runMedium = med
	if _, err := a.validate(med); err != nil {
		return err
	}
	cfg := med.Config
	if len(cfg.Mappings) == 0 {
		return fmt.Errorf("keine Mappings konfiguriert (%s)", filepath.Join(med.Path, config.FileName))
	}
	rvv, rv, err := rsync.Version(ctx)
	if err != nil {
		return err
	}
	if !rsync.AtLeast(rvv, rsync.MinVersion) {
		return fmt.Errorf("rsync %s ist zu alt, benötigt wird mindestens %d.%d.%d", rv, rsync.MinVersion[0], rsync.MinVersion[1], rsync.MinVersion[2])
	}
	selected, err := a.selectMappings(cfg)
	if err != nil {
		return err
	}
	needGit := false
	for _, m := range selected {
		needGit = needGit || m.IsGit()
	}
	gitVersion := ""
	if needGit {
		if gitVersion, err = gitfilter.Version(ctx); err != nil {
			return err
		}
	}

	r := &runCtx{a: a, med: med, cfg: cfg, dryRun: a.Opt.DryRun, fsync: rsync.AtLeast(rvv, rsync.FsyncVersion)}
	logName := start.Format("2006-01-02_150405") + "_" + state.CleanHost(a.Host)
	if !r.dryRun {
		for _, d := range []string{med.MirrorDir, med.LogDir} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
		lk, err := a.acquireLock(ctx, med, "run")
		if err != nil {
			return err
		}
		defer func() {
			if err := lk.Release(); err != nil {
				a.UI.Warn("Lock entfernen: %v", err)
			}
		}()
		r.log, err = createLog(med.LogDir, logName)
		if err != nil {
			return fmt.Errorf("Log anlegen: %w", err)
		}
		defer func() {
			r.log.Section("Ende")
			if retErr != nil {
				r.log.Printf("Ergebnis: %v (Exit-Code %d)", retErr, Code(retErr))
			} else {
				r.log.Printf("Ergebnis: Erfolg, %d Warnungen", r.count)
			}
			if err := r.log.Close(); err != nil {
				a.UI.Warn("Log schließen: %v", err)
			}
		}()
		// Data of an earlier run may still be on its way to the stick; all
		// checks below would silently wait behind it.
		if medium.PendingWrites() > 32<<20 {
			r.log.Printf("Ausstehende Schreibvorgänge vor dem Start: %s", ui.Bytes(medium.PendingWrites()))
			if err := a.waitForWrites(ctx, "Ein früherer Schreibvorgang läuft noch"); err != nil {
				return err
			}
		}
	} else {
		a.UI.Hint("Probelauf (--dry-run): es wird nichts auf das Medium geschrieben.")
	}
	r.writeLogHeader(selected, rv, gitVersion)

	tmp, err := gitfilter.TempDir()
	if err != nil {
		return err
	}
	r.tmpDir = tmp
	defer os.RemoveAll(tmp)

	if r.old, err = state.Read(med.Path); err != nil {
		r.warn(nil, "Statusdatei unlesbar, Prüfung „anderer Rechner“ entfällt: %v", err)
	}
	if r.old != nil && r.old.Status != state.StatusComplete {
		a.UI.Hint("Der letzte Lauf (%s von %s) wurde nicht abgeschlossen; dieser Lauf bringt den Spiegel wieder auf Stand.", fmtTime(r.old.StartedAt), r.old.Host)
	}

	// Sources
	for i, m := range selected {
		j, err := r.checkSource(m)
		if err != nil {
			r.log.Printf("FEHLER: %v", err)
			return err
		}
		if r.old != nil {
			j.prev = r.old.Mapping(j.target)
		}
		if !j.mirrorExists() {
			a.UI.Hint("%s: noch nicht im Spiegel, alles wird kopiert.", m.Name)
		}
		if m.IsGit() {
			if err := r.buildGitList(ctx, j, i); err != nil {
				return err
			}
		}
		r.jobs = append(r.jobs, j)
	}

	if a.Opt.Checksum {
		a.UI.Hint("--checksum: Inhalte werden verglichen, alle Dateien werden gelesen – das dauert länger.")
	}
	a.UI.Info("Prüfe Quellen und vergleiche mit dem Stick …")
	if err := r.checkCaseCollisions(ctx); err != nil {
		return err
	}
	if err := r.safetyChecks(ctx); err != nil {
		return err
	}
	if r.dryRun {
		r.printDryRun()
		if r.count > 0 {
			return &ExitErr{Code: ExitWarnings}
		}
		return nil
	}

	// Write the mirror.
	r.st = &state.State{
		FormatVersion: state.FormatVersion, ToolVersion: Version, MediumID: cfg.Medium.ID,
		Host: a.Host, User: a.User, StartedAt: start.UTC(), Status: state.StatusRunning,
		Warnings: r.warnings,
	}
	r.st.Mappings = make([]state.Mapping, len(r.jobs), len(r.jobs)+len(cfg.Mappings))
	running := map[string]bool{}
	for i, j := range r.jobs {
		r.st.Mappings[i] = *j.ms
		j.ms = &r.st.Mappings[i]
		running[j.target] = true
	}
	if r.old != nil {
		// --only: keep what is known about the other mappings
		for _, mm := range r.old.Mappings {
			if !running[filepath.Clean(mm.Target)] {
				r.st.Mappings = append(r.st.Mappings, mm)
			}
		}
	}
	if err := r.save(); err != nil {
		return err
	}
	for _, j := range r.jobs {
		if err := r.copyMapping(ctx, j); err != nil {
			status := state.StatusFailed
			if ctx.Err() != nil {
				status = state.StatusAborted
				err = ui.ErrAborted
				a.UI.EndProgress()
				a.UI.Error("Abbruch – der Spiegel ist jetzt nur teilweise aktualisiert. Der nächste Lauf bringt ihn wieder auf Stand.")
			}
			for _, k := range r.jobs {
				if k.ms.Status == "" {
					k.ms.Status = state.MappingSkipped
				}
			}
			r.st.Status = status
			now := time.Now().UTC()
			r.st.FinishedAt = &now
			if serr := r.save(); serr != nil {
				a.UI.Warn("Statusdatei speichern: %v", serr)
			}
			return err
		}
	}
	// Only report success once the data really is on the stick.
	t0 := time.Now()
	if err := a.waitForWrites(ctx, "Schreibe Daten auf den Stick"); err != nil {
		r.st.Status = state.StatusAborted
		_ = r.save()
		return ui.ErrAborted
	}
	r.log.Printf("Alle Daten auf den Stick geschrieben (%s)", time.Since(t0).Round(time.Second))
	now := a.Now().UTC()
	r.st.FinishedAt = &now
	r.st.Status = state.StatusComplete
	if err := r.save(); err != nil {
		return err
	}
	if err := state.SyncDir(med.Path); err != nil {
		r.warn(nil, "fsync: %v", err)
	}
	r.cleanLogs(logName)
	r.summary(a.Now().Sub(start))
	if r.count > 0 {
		return &ExitErr{Code: ExitWarnings}
	}
	return nil
}

func createLog(dir, name string) (*logx.Log, error) {
	for i := 1; ; i++ {
		n := name
		if i > 1 {
			n = fmt.Sprintf("%s-%d", name, i)
		}
		l, err := logx.Create(filepath.Join(dir, n+".log"))
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return l, err
	}
}

// cleanLogs removes logs older than LogMaxAge.
func (r *runCtx) cleanLogs(current string) {
	entries, err := os.ReadDir(r.med.LogDir)
	if err != nil {
		return
	}
	now := r.a.Now()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".log") || strings.HasPrefix(e.Name(), current) {
			continue
		}
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > LogMaxAge {
			if os.Remove(filepath.Join(r.med.LogDir, e.Name())) == nil {
				r.log.Printf("altes Log gelöscht: %s", e.Name())
			}
		}
	}
}

func (r *runCtx) save() error {
	if err := state.Write(r.med.Path, r.st); err != nil {
		return fmt.Errorf("Statusdatei schreiben: %w", err)
	}
	return nil
}

func (a *App) selectMappings(cfg *config.Config) ([]*config.Mapping, error) {
	var out []*config.Mapping
	if len(a.Opt.Only) == 0 {
		for i := range cfg.Mappings {
			out = append(out, &cfg.Mappings[i])
		}
		return out, nil
	}
	for _, n := range a.Opt.Only {
		found := false
		for i := range cfg.Mappings {
			if strings.EqualFold(cfg.Mappings[i].Name, strings.TrimSpace(n)) {
				dup := false
				for _, o := range out {
					dup = dup || o == &cfg.Mappings[i]
				}
				if !dup {
					out = append(out, &cfg.Mappings[i])
				}
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("--only: Mapping %q gibt es nicht (vorhanden: %s)", n, strings.Join(cfg.MappingNames(), ", "))
		}
	}
	return out, nil
}

func (r *runCtx) writeLogHeader(selected []*config.Mapping, rsyncVersion, gitVersion string) {
	if r.log == nil {
		return
	}
	a := r.a
	r.log.Section("stick-backup " + Version)
	r.log.Printf("Host: %s, Benutzer: %s", a.Host, a.User)
	r.log.Printf("Medium: %s (%s), %s, Dateisystem %s", r.cfg.Medium.Name, r.cfg.Medium.ID, r.med.Path, r.med.FSType)
	r.log.Printf("rsync %s, git %s", rsyncVersion, gitVersion)
	r.log.Printf("max_delete_percent=%d, allow_empty_source=%v", r.cfg.Safety.MaxDeletePercent, r.cfg.Safety.AllowEmptySource)
	for _, m := range selected {
		r.log.Printf("Mapping %s: %s -> %s, filter=%s, exclude=%q", m.Name, m.Source, m.CleanTarget(), m.Filter, r.cfg.Excludes(m))
		if m.IsGit() {
			r.log.Printf("  include=%q backup_git_dirs=%v", m.Include, m.GitDirs())
		} else if len(m.Include) > 0 {
			r.log.Printf("  nur (include)=%q", m.Include)
		}
	}
	if len(selected) < len(r.cfg.Mappings) {
		r.log.Printf("Nur ausgewählte Mappings (--only)")
	}
	if r.a.Opt.Checksum {
		r.log.Printf("Vergleich nach Inhalt (--checksum)")
	}
}

// checkSource checks that the source exists, is a readable directory and is
// not empty (unless allowed).
func (r *runCtx) checkSource(m *config.Mapping) (*job, error) {
	src, err := config.ExpandSource(m.Source, r.a.Home)
	if err != nil {
		return nil, fmt.Errorf("Mapping %s: %w", m.Name, err)
	}
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return nil, fmt.Errorf("Mapping %s: Quelle %s fehlt: %w", m.Name, src, err)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("Mapping %s: %w", m.Name, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("Mapping %s: Quelle %s ist kein Ordner", m.Name, resolved)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("Mapping %s: Quelle nicht lesbar: %w", m.Name, err)
	}
	_, err = f.Readdirnames(1)
	f.Close()
	if errors.Is(err, io.EOF) {
		if !r.cfg.Safety.AllowEmptySource {
			return nil, fmt.Errorf("Mapping %s: Quelle %s ist leer (ist ein Laufwerk nicht eingehängt? Sonst safety.allow_empty_source = true)", m.Name, resolved)
		}
		r.warn(nil, "Mapping %s: Quelle %s ist leer", m.Name, resolved)
	} else if err != nil {
		return nil, fmt.Errorf("Mapping %s: Quelle nicht lesbar: %w", m.Name, err)
	}
	r.log.Printf("Quelle %s: %s ok", m.Name, resolved)
	j := newJob(r.cfg, m, resolved, r.med.MirrorDir)
	j.ms = &state.Mapping{Name: m.Name, Source: resolved, Target: j.target, Filter: m.Filter}
	if miss := j.missingIncludes(); len(miss) > 0 {
		r.warn(nil, "%s: include-Pfade gibt es in der Quelle nicht (Tippfehler?):%s", m.Name, examples(miss, 10))
	}
	return j, nil
}

func (r *runCtx) buildGitList(ctx context.Context, j *job, idx int) error {
	dir := filepath.Join(r.tmpDir, fmt.Sprintf("m%d", idx))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	r.a.UI.Info("%s: Dateiliste über Git erstellen …", j.m.Name)
	t0 := time.Now()
	res, err := gitfilter.BuildList(ctx, gitfilter.Options{
		Root: j.src, Excludes: j.excludes, Includes: j.m.Include, GitDirs: j.m.GitDirs(), TmpDir: dir,
	})
	if err != nil {
		return fmt.Errorf("Mapping %s: Dateiliste: %w", j.m.Name, err)
	}
	r.log.Section("Git-Dateiliste " + j.m.Name)
	for _, ri := range res.Repos {
		kind := "Ordner"
		if ri.IsRepo {
			kind = "Repo"
		}
		line := fmt.Sprintf("%s %s: %d Dateien", kind, ri.Path, ri.Files)
		if len(j.m.Include) > 0 {
			line += fmt.Sprintf(", include %s", ri.IncludeTime.Round(time.Millisecond))
		}
		r.log.Printf("%s", line)
	}
	r.log.Printf("%d Dateien in %s", len(res.Files), time.Since(t0).Round(time.Millisecond))
	for _, w := range res.Warnings {
		r.warn(nil, "%s: %s", j.m.Name, w)
	}
	j.files = res.Files
	j.listPath = filepath.Join(dir, "files")
	return gitfilter.WriteList(j.listPath, j.files)
}

// runRsync runs rsync, logging the command line, output and exit code.
func (r *runCtx) runRsync(ctx context.Context, label string, o rsync.Options, h rsync.Handlers) (*rsync.Result, error) {
	args := rsync.Args(o)
	r.log.Printf("%s: %s", label, rsync.QuoteArgs(args))
	inner := h
	h.Line = func(s string) {
		r.log.Raw(s)
		if inner.Line != nil {
			inner.Line(s)
		}
	}
	h.Stderr = func(s string) {
		r.log.Raw("stderr: " + s)
		if inner.Stderr != nil {
			inner.Stderr(s)
		}
	}
	res, err := rsync.Run(ctx, args, h)
	if err != nil {
		r.log.Printf("%s: %v", label, err)
		return res, err
	}
	r.log.Printf("%s: Exit-Code %d", label, res.ExitCode)
	return res, nil
}

// stderrPaths extracts the paths rsync complained about (relative to the
// mapping), from quoted absolute paths below src or dst. Temporary receiver
// files (".name.XXXXXX", e.g. mkstemp failures on exFAT) are mapped back to
// the real name.
var quotedRe = regexp.MustCompile(`"(/[^"]+)"`)
var tmpNameRe = regexp.MustCompile(`^\.(.+)\.[A-Za-z0-9]{6}$`)

func stderrPaths(lines []string, src, dst string) map[string]bool {
	out := map[string]bool{}
	for _, l := range lines {
		for _, m := range quotedRe.FindAllStringSubmatch(l, -1) {
			if rel, err := filepath.Rel(src, m[1]); err == nil && !strings.HasPrefix(rel, "..") {
				out[rel] = true
			} else if rel, err := filepath.Rel(dst, m[1]); err == nil && !strings.HasPrefix(rel, "..") {
				if t := tmpNameRe.FindStringSubmatch(filepath.Base(rel)); t != nil {
					rel = filepath.Join(filepath.Dir(rel), t[1])
				}
				out[rel] = true
			}
		}
	}
	return out
}

// copyMapping updates the mirror of one mapping and checks afterwards
// whether the source changed while copying.
func (r *runCtx) copyMapping(ctx context.Context, j *job) error {
	a := r.a
	r.log.Section("Mapping " + j.m.Name)
	a.UI.Heading("%s: %s → %s", j.m.Name, j.src, filepath.Join("mirror", j.target))
	fail := func(err error) error {
		j.ms.Status = state.MappingFailed
		j.ms.Host = a.Host // partially written by this host
		now := a.Now().UTC()
		j.ms.UpdatedAt = &now
		return err
	}
	if err := os.MkdirAll(filepath.Dir(j.dst), 0o755); err != nil {
		return fail(fmt.Errorf("Mapping %s: %w", j.m.Name, err))
	}
	o := j.opts(j.src, j.dst)
	o.Delete, o.DeleteExcluded = !j.m.IsGit(), !j.m.IsGit()
	o.Stats, o.Progress, o.Fsync = true, true, r.fsync
	o.Checksum = a.Opt.Checksum
	var skipped []string
	res, err := r.runRsync(ctx, "rsync", o, rsync.Handlers{
		Line: func(l string) {
			if p, ok := rsync.SkippedNonRegular(l); ok {
				skipped = append(skipped, p)
			}
		},
		Item: func(it rsync.Item) {
			r.log.Raw(it.String())
			if a.UI.Verbose {
				a.UI.Detail("%s", it.String())
			}
		},
		Progress: func(s string) { a.UI.Progress(j.m.Name + ": " + s) },
		Stderr:   func(s string) { a.UI.EndProgress() },
	})
	a.UI.EndProgress()
	if err != nil {
		return fail(err)
	}
	now := a.Now().UTC()
	j.ms.Host, j.ms.UpdatedAt = a.Host, &now
	j.ms.FilesTotal = res.Stats.RegFiles // symlinks/devices are skipped
	j.ms.FilesTransferred = res.Stats.FilesTransferred
	j.ms.BytesTransferred = res.Stats.TransferredSize
	j.ms.Status = state.MappingOK
	switch res.ExitCode {
	case 0:
	case 24:
		r.warn(j, "%s: Dateien sind während der Übertragung verschwunden (rsync-Code 24)", j.m.Name)
	case 23:
		var lines []string
		for _, l := range res.Stderr {
			if !strings.HasPrefix(l, "rsync error:") {
				lines = append(lines, l)
			}
		}
		all := strings.Join(lines, "\n")
		hint := ""
		if strings.Contains(all, "Invalid argument") {
			hint = "\n  (exFAT/FAT erlauben in Namen keine Zeichen \" * : < > ? \\ | – solche Dateien umbenennen oder ausschließen)"
		}
		if strings.Contains(all, "File too large") {
			hint += "\n  (FAT32 kann keine Dateien über 4 GB speichern)"
		}
		r.warn(j, "%s: nicht alle Dateien übertragen (rsync-Code 23):%s%s", j.m.Name, examples(lines, 10), hint)
	default:
		return fail(fmt.Errorf("Mapping %s: rsync fehlgeschlagen mit Code %d:\n  %s", j.m.Name, res.ExitCode, strings.Join(res.Stderr, "\n  ")))
	}
	if len(skipped) > 0 {
		r.log.Printf("%s: %d Symlinks/Sonderdateien übersprungen", j.m.Name, len(skipped))
		a.UI.Hint("  Hinweis: %d Symlinks übersprungen (werden nicht gesichert):%s", len(skipped), examples(skipped, 5))
	}
	if j.m.IsGit() {
		n, err := removeUnlisted(j.dst, j.files)
		if err != nil {
			r.warn(j, "%s: nicht mehr ausgewählte Dateien entfernen: %v", j.m.Name, err)
		}
		j.ms.FilesTotal = int64(len(j.files))
		r.log.Printf("%s: %d nicht mehr ausgewählte Dateien aus dem Spiegel entfernt", j.m.Name, n)
	}
	if err := r.save(); err != nil {
		return err
	}

	// Changed while copying?
	skip := stderrPaths(res.Stderr, j.src, j.dst)
	var changed []string
	co := j.opts(j.src, j.dst)
	co.DryRun = true
	cres, err := r.runRsync(ctx, "Nachkontrolle", co, rsync.Handlers{Item: func(it rsync.Item) {
		r.log.Raw(it.String())
		if it.IsDirTimeOnly() || skip[strings.TrimSuffix(it.Path, "/")] {
			return
		}
		changed = append(changed, it.Path)
	}})
	if err != nil {
		return err
	}
	if cres.ExitCode != 0 && !cres.ExitWarning() {
		r.warn(j, "%s: Nachkontrolle fehlgeschlagen (rsync-Code %d)", j.m.Name, cres.ExitCode)
	}
	if len(changed) > 0 {
		r.warn(j, "%s: Quelle hat sich während des Kopierens geändert (lief ein Programm, das diese Dateien benutzt?):%s", j.m.Name, examples(changed, 10))
	}
	a.UI.Success("  %d Dateien übertragen (%s), %d insgesamt", j.ms.FilesTransferred, ui.Bytes(j.ms.BytesTransferred), j.ms.FilesTotal)
	return r.save()
}

// safetyChecks runs before anything is written: many deletions, mirror
// written by another computer, free space.
func (r *runCtx) safetyChecks(ctx context.Context) error {
	a := r.a
	cfg := r.cfg
	r.log.Section("Sicherheitsprüfungen")
	var questions []string

	for _, j := range r.jobs {
		if !j.mirrorExists() {
			continue
		}
		// a) another computer wrote this part of the mirror last
		if p := j.prev; p != nil && p.Host != "" && p.Host != a.Host && p.UpdatedAt != nil {
			newer, err := r.countNewer(ctx, j)
			if err != nil {
				return err
			}
			r.log.Printf("%s: zuletzt von %s gesichert, %d Dateien dort neuer oder hier nicht vorhanden", j.m.Name, p.Host, len(newer))
			if len(newer) > 0 {
				r.warn(nil, "%s wurde zuletzt von %s gesichert (%s). Im Spiegel sind %d Dateien neuer als hier oder fehlen hier – beim Sichern werden sie überschrieben bzw. gelöscht:%s",
					j.m.Name, p.Host, fmtTime(*p.UpdatedAt), len(newer), examples(newer, 10))
				questions = append(questions, j.m.Name+": "+p.Host+" ist neuer")
			}
		}
		// b) many deletions
		deleted, total, err := r.countDeletions(ctx, j)
		if err != nil {
			return err
		}
		j.ms.FilesDeleted = int64(len(deleted))
		pct := 0.0
		if total > 0 {
			pct = float64(len(deleted)) * 100 / float64(total)
		} else if len(deleted) > 0 {
			pct = 100
		}
		r.log.Printf("%s: %d von %d Dateien im Spiegel würden gelöscht (%.1f %%)", j.m.Name, len(deleted), total, pct)
		if len(deleted) > 0 && pct > float64(cfg.Safety.MaxDeletePercent) {
			r.warn(nil, "%s: %d von %d Dateien (%.0f %%) würden im Spiegel gelöscht (Grenze %d %%):%s",
				j.m.Name, len(deleted), total, pct, cfg.Safety.MaxDeletePercent, examples(deleted, 10))
			questions = append(questions, j.m.Name+": viele Löschungen")
		}
	}

	// c) free space (simulation, also used for the --dry-run summary)
	var need int64
	for _, j := range r.jobs {
		o := j.opts(j.src, j.dst)
		o.DryRun, o.Stats = true, true
		o.Delete, o.DeleteExcluded = !j.m.IsGit(), !j.m.IsGit()
		o.Checksum = a.Opt.Checksum
		res, err := r.runRsync(ctx, j.m.Name+": Simulation", o, rsync.Handlers{Item: func(it rsync.Item) {
			if it.IsDelete() && !it.IsDir() {
				j.simDel++
			}
			if r.dryRun && a.UI.Verbose {
				a.UI.Detail("  %s", it.String())
			}
		}})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 && !res.ExitWarning() {
			return fmt.Errorf("Mapping %s: rsync-Simulation fehlgeschlagen mit Code %d:\n  %s", j.m.Name, res.ExitCode, strings.Join(res.Stderr, "\n  "))
		}
		j.sim = res
		need += res.Stats.TransferredSize
	}
	_, free, err := medium.Space(r.med.Path)
	if err != nil {
		return fmt.Errorf("freier Platz: %w", err)
	}
	needRes := need + need/20
	r.log.Printf("Platzbedarf: %s (+5 %% = %s), frei: %s", ui.Bytes(need), ui.Bytes(needRes), ui.Bytes(int64(free)))
	if uint64(needRes) > free {
		err := fmt.Errorf("nicht genug Platz auf dem Medium: benötigt %s (inkl. 5 %% Reserve), frei %s. Mappings verkleinern (exclude) oder einen größeren Stick verwenden", ui.Bytes(needRes), ui.Bytes(int64(free)))
		r.log.Printf("FEHLER: %v", err)
		return err
	}

	if len(questions) > 0 && !r.dryRun {
		if err := r.confirm(ctx, "Trotzdem sichern?"); err != nil {
			return err
		}
	}
	return nil
}

// countDeletions returns the files the run would delete from the mirror and
// the number of files currently in it.
func (r *runCtx) countDeletions(ctx context.Context, j *job) ([]string, int64, error) {
	var deleted []string
	if j.m.IsGit() {
		tree, err := gitfilter.ListTree(j.dst)
		if err != nil {
			return nil, 0, fmt.Errorf("Spiegel lesen: %w", err)
		}
		cur := make(map[string]bool, len(j.files))
		for _, f := range j.files {
			cur[f] = true
		}
		for p := range tree {
			if !cur[p] {
				deleted = append(deleted, p)
			}
		}
		sort.Strings(deleted)
		return deleted, int64(len(tree)), nil
	}
	o := j.opts(j.src, j.dst)
	o.DryRun, o.Delete, o.DeleteExcluded = true, true, true
	res, err := r.runRsync(ctx, j.m.Name+": Löschprüfung", o, rsync.Handlers{Item: func(it rsync.Item) {
		if it.IsDelete() && !it.IsDir() {
			deleted = append(deleted, it.Path)
		}
	}})
	if err != nil {
		return nil, 0, err
	}
	if res.ExitCode != 0 && !res.ExitWarning() {
		return nil, 0, fmt.Errorf("Mapping %s: Löschprüfung fehlgeschlagen (rsync-Code %d)", j.m.Name, res.ExitCode)
	}
	var total int64
	if j.prev != nil && j.prev.FilesTotal > 0 {
		total = j.prev.FilesTotal
	} else {
		tree, err := gitfilter.ListTree(j.dst)
		if err != nil {
			return nil, 0, fmt.Errorf("Spiegel lesen: %w", err)
		}
		total = int64(len(tree))
	}
	return deleted, total, nil
}

// countNewer returns files in the mirror that are newer than in the source
// or missing in the source.
func (r *runCtx) countNewer(ctx context.Context, j *job) ([]string, error) {
	var newer []string
	if j.m.IsGit() {
		tree, err := gitfilter.ListTree(j.dst)
		if err != nil {
			return nil, fmt.Errorf("Spiegel lesen: %w", err)
		}
		for p, fi := range tree {
			sfi, err := os.Lstat(filepath.Join(j.src, p))
			// 1 s tolerance like rsync --modify-window=1 (exFAT/FAT precision)
			if err != nil || fi.ModTime().After(sfi.ModTime().Add(time.Second)) {
				newer = append(newer, p)
			}
		}
		sort.Strings(newer)
		return newer, nil
	}
	o := j.opts(j.dst, j.src)
	o.DryRun, o.Update = true, true
	res, err := r.runRsync(ctx, j.m.Name+": Vergleich mit Spiegel", o, rsync.Handlers{Item: func(it rsync.Item) {
		if !it.IsDir() && !it.IsDelete() {
			newer = append(newer, it.Path)
		}
	}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 && !res.ExitWarning() {
		return nil, fmt.Errorf("Mapping %s: Vergleich mit dem Spiegel fehlgeschlagen (rsync-Code %d)", j.m.Name, res.ExitCode)
	}
	return newer, nil
}

// checkCaseCollisions warns about paths that differ only in case: on exFAT/
// FAT/NTFS they are the same file, and one of them silently overwrites the
// other. rsync does not notice this.
func (r *runCtx) checkCaseCollisions(ctx context.Context) error {
	for _, j := range r.jobs {
		paths, err := j.selectedPaths(ctx)
		if err != nil {
			return fmt.Errorf("Mapping %s: %w", j.m.Name, err)
		}
		if c := caseCollisions(paths); len(c) > 0 {
			var lines []string
			for _, g := range c {
				lines = append(lines, strings.Join(g, "  ↔  "))
			}
			r.warn(nil, "%s: %d Namen unterscheiden sich nur in Groß-/Kleinschreibung. Auf %s ist das dieselbe Datei, nur eine davon landet im Spiegel – bitte umbenennen:%s",
				j.m.Name, len(c), r.med.FSType, examples(lines, 10))
		}
	}
	return nil
}

// selectedPaths lists files and directories selected for the mapping.
func (j *job) selectedPaths(ctx context.Context) ([]string, error) {
	if j.m.IsGit() {
		return withParents(j.files), nil
	}
	return rsync.ListFiles(ctx, j.src, j.excludes, j.includes)
}

// caseCollisions groups paths that are equal when case is ignored.
func caseCollisions(paths []string) [][]string {
	groups := map[string][]string{}
	for _, p := range paths {
		k := strings.ToLower(p)
		groups[k] = append(groups[k], p)
	}
	var out [][]string
	for _, g := range groups {
		if len(g) > 1 {
			sort.Strings(g)
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// withParents adds all parent directories of the given file paths.
func withParents(files []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f)
		for d := filepath.Dir(f); d != "." && !seen[d]; d = filepath.Dir(d) {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out
}

// removeUnlisted deletes files below dir that are not in files (git filter,
// where rsync --files-from cannot delete) and then empty directories. It
// returns the number of removed files.
func removeUnlisted(dir string, files []string) (int, error) {
	keep := make(map[string]bool, len(files))
	for _, f := range files {
		keep[f] = true
	}
	n := 0
	var dirs []string
	var errs []error
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if rel != "." {
				dirs = append(dirs, p)
			}
			return nil
		}
		if !keep[rel] {
			if err := os.Remove(p); err != nil {
				errs = append(errs, err)
			} else {
				n++
			}
		}
		return nil
	})
	if err != nil {
		errs = append(errs, err)
	}
	// deepest first; Remove fails on non-empty directories, which is fine
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
	return n, errors.Join(errs...)
}

func (r *runCtx) printDryRun() {
	a := r.a
	a.UI.Println("")
	a.UI.Heading("Probelauf – Zusammenfassung")
	var total int64
	for _, j := range r.jobs {
		st := j.sim.Stats
		total += st.TransferredSize
		line := fmt.Sprintf("  %-16s %d von %d Dateien würden übertragen (%s)", j.m.Name, st.FilesTransferred, st.RegFiles, ui.Bytes(st.TransferredSize))
		if j.mirrorExists() {
			line += fmt.Sprintf(", %d würden im Spiegel gelöscht", max(int64(j.simDel), j.ms.FilesDeleted))
		} else {
			line += ", noch nicht im Spiegel"
		}
		a.UI.Println(line)
	}
	a.UI.Printf("  Insgesamt %s neu auf dem Medium.", ui.Bytes(total))
}

func (r *runCtx) summary(d time.Duration) {
	a := r.a
	a.UI.Println("")
	a.UI.Heading("Zusammenfassung")
	r.log.Section("Zusammenfassung")
	for _, j := range r.jobs {
		ms := j.ms
		line := fmt.Sprintf("  %-16s %-14s %d Dateien übertragen (%s), %d Warnungen", ms.Name, statusText(ms.Status), ms.FilesTransferred, ui.Bytes(ms.BytesTransferred), len(ms.Warnings))
		a.UI.Println(line)
		r.log.Printf("%s", strings.TrimSpace(line))
	}
	line := fmt.Sprintf("Spiegel %s aktualisiert in %s", r.med.MirrorDir, fmtDuration(d))
	r.log.Printf("%s, %d Warnungen", line, r.count)
	if r.count > 0 {
		a.UI.Warn("%s – mit %d Warnungen, siehe Log %s", line, r.count, r.log.Path)
	} else {
		a.UI.Success("%s – erfolgreich.", line)
	}
}
