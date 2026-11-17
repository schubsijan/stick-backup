// Package app implements the commands of the backup tool.
//
// The tool keeps one mirror of the last state on a removable medium
// (typically an exFAT stick): <medium>/mirror/<target>/…, plus the status
// file .backup-state.json and logs/. There are no older versions.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/schubsijan/stick-backup/internal/config"
	"github.com/schubsijan/stick-backup/internal/lock"
	"github.com/schubsijan/stick-backup/internal/medium"
	"github.com/schubsijan/stick-backup/internal/state"
	"github.com/schubsijan/stick-backup/internal/ui"
)

// Version of the tool.
const Version = "0.2.0"

// Exit codes.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitWarnings = 2
	ExitAborted  = 3
	ExitNoMedium = 4
	ExitLocked   = 5
)

// ExitErr carries an exit code. Err may be nil (e.g. success with warnings).
type ExitErr struct {
	Code int
	Err  error
}

func (e *ExitErr) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitErr) Unwrap() error { return e.Err }

func exitf(code int, format string, a ...any) error {
	return &ExitErr{Code: code, Err: fmt.Errorf(format, a...)}
}

// Code maps an error returned by a command to an exit code.
func Code(err error) int {
	var ee *ExitErr
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &ee):
		return ee.Code
	case errors.Is(err, ui.ErrAborted), errors.Is(err, context.Canceled):
		return ExitAborted
	}
	return ExitError
}

// Options are the parsed command line flags.
type Options struct {
	Target      string
	Yes         bool
	Verbose     bool
	DryRun      bool
	Checksum    bool // run: compare content instead of size+mtime
	Only        []string
	ForceUnlock bool
}

// App holds the environment of one invocation.
type App struct {
	UI   *ui.UI
	Opt  Options
	Host string // static host name
	// HostErr is set when no static host name exists; commands that need
	// the host fail with it (see requireHost).
	HostErr error
	User    string
	Home    string
	Now     func() time.Time
	// Mounts returns the mount table (replaceable in tests).
	Mounts func() ([]medium.Mount, error)
	// Drives returns removable drives, mounted or not (replaceable in tests).
	Drives func(context.Context) []medium.Drive
	// MountDrive mounts a device and returns the mount point (replaceable).
	MountDrive func(ctx context.Context, device string) (string, error)
	// Sync flushes pending writes to all disks (replaceable in tests).
	Sync func()
	// UnmountDrive unmounts a device (replaceable in tests).
	UnmountDrive func(ctx context.Context, device string) error
	// runMedium is the medium of the current run (for the final eject).
	runMedium *Medium
}

// New creates an App for the current process.
func New(u *ui.UI, opt Options) (*App, error) {
	a := &App{UI: u, Opt: opt, Now: time.Now, Mounts: medium.ReadMounts,
		Drives: medium.RemovableDrives, MountDrive: medium.MountDrive, Sync: medium.SyncAll,
		UnmountDrive: medium.UnmountDrive}
	if h, err := StaticHostname(HostnameFile); err != nil {
		a.HostErr = err
	} else {
		a.Host = h
	}
	if cu, err := user.Current(); err == nil {
		a.User, a.Home = cu.Username, cu.HomeDir
	}
	if h, err := os.UserHomeDir(); err == nil {
		a.Home = h
	}
	if a.User == "" {
		a.User = strconv.Itoa(os.Getuid())
	}
	return a, nil
}

// HostnameFile holds the static host name (systemd: hostnamectl set-hostname).
var HostnameFile = "/etc/hostname"

// ParseHostnameFile returns the host name from /etc/hostname content:
// the first non-empty line that is not a comment.
func ParseHostnameFile(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

// StaticHostname reads the static host name. The transient name from
// os.Hostname() is not used: it may be assigned via DHCP and change with the
// network, so the tool could not tell whether the mirror was written by this
// computer or another one.
func StaticHostname(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%s lesen: %w", path, err)
	}
	if h := ParseHostnameFile(data); h != "" {
		return h, nil
	}
	cur, _ := os.Hostname()
	return "", fmt.Errorf("kein fester Hostname gesetzt (%s ist leer oder fehlt). Der aktuelle Name %q kann vom Netzwerk vergeben sein und sich ändern; das Tool könnte dann nicht erkennen, welcher Rechner den Spiegel zuletzt geschrieben hat.\nBitte einmalig setzen, z.B.: sudo hostnamectl set-hostname laptop", path, cur)
}

// requireHost fails if no static host name is known.
func (a *App) requireHost() error { return a.HostErr }

// Medium is a selected backup medium.
type Medium struct {
	Path      string
	FSType    string
	Device    string
	Config    *config.Config
	MirrorDir string
	LogDir    string
}

func newMedium(path, fstype, device string, cfg *config.Config) *Medium {
	return &Medium{Path: path, FSType: fstype, Device: device, Config: cfg,
		MirrorDir: filepath.Join(path, "mirror"), LogDir: filepath.Join(path, "logs")}
}

// selectMedium finds the medium (mounted file system with backup.toml).
func (a *App) selectMedium(ctx context.Context) (*Medium, error) {
	mounts, err := a.Mounts()
	if err != nil {
		return nil, fmt.Errorf("Einhängepunkte lesen: %w", err)
	}
	var med *Medium
	if a.Opt.Target != "" {
		p, err := filepath.Abs(a.Opt.Target)
		if err != nil {
			return nil, err
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		cfgPath := filepath.Join(p, config.FileName)
		if _, err := os.Stat(cfgPath); err != nil {
			return nil, exitf(ExitNoMedium, "%s nicht gefunden – ist das ein Backup-Medium? (Neues Medium: stick-backup init %s)", cfgPath, p)
		}
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cfgPath, err)
		}
		m, _ := medium.MountFor(mounts, p)
		med = newMedium(p, m.FSType, m.Source, cfg)
	} else {
		cands, warns := medium.FindCandidates(mounts)
		if len(cands) == 0 {
			// Sticks are not always mounted automatically: offer to mount.
			mounted, err := a.mountUnmounted(ctx)
			if err != nil {
				return nil, err
			}
			if mounted {
				if mounts, err = a.Mounts(); err != nil {
					return nil, fmt.Errorf("Einhängepunkte lesen: %w", err)
				}
				cands, warns = medium.FindCandidates(mounts)
			}
		}
		for _, w := range warns {
			a.UI.Warn("ungültige Konfiguration: %s", w)
		}
		switch {
		case len(cands) == 0:
			a.UI.Error("Kein Backup-Medium gefunden.")
			a.UI.Hint("Stick einstecken – oder mit --target <pfad> angeben. Neuer Stick: stick-backup init")
			return nil, &ExitErr{Code: ExitNoMedium}
		case len(cands) == 1:
			c := cands[0]
			med = newMedium(c.Path, c.FSType, c.Device, c.Config)
			a.UI.Heading("Backup-Medium gefunden:")
			a.showMediumInfo(med)
			a.UI.Println("")
			ok, err := a.UI.Confirm(ctx, "Als Ziel verwenden?")
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, ui.ErrAborted
			}
		default:
			if a.UI.Yes {
				return nil, exitf(ExitError, "%d Backup-Medien gefunden, mit --yes bitte --target angeben", len(cands))
			}
			a.UI.Heading("%d Backup-Medien gefunden:", len(cands))
			for i, c := range cands {
				a.UI.Println("")
				a.UI.Heading("[%d]", i+1)
				a.showMediumInfo(newMedium(c.Path, c.FSType, c.Device, c.Config))
			}
			a.UI.Println("")
			i, err := a.UI.Choose(ctx, "Welches Medium verwenden?", len(cands))
			if err != nil {
				return nil, err
			}
			c := cands[i]
			med = newMedium(c.Path, c.FSType, c.Device, c.Config)
		}
	}
	if w := medium.CheckFS(med.FSType); w != "" {
		a.UI.Warn("%s", w)
	}
	return med, nil
}

// mountUnmounted offers to mount removable drives that are plugged in but
// not mounted. It reports whether at least one drive was mounted.
func (a *App) mountUnmounted(ctx context.Context) (bool, error) {
	if a.Drives == nil || a.MountDrive == nil {
		return false, nil
	}
	var todo []medium.Drive
	for _, d := range a.Drives(ctx) {
		if !d.Mounted() {
			todo = append(todo, d)
		}
	}
	if len(todo) == 0 {
		return false, nil
	}
	a.UI.Heading("Nicht eingehängte Wechseldatenträger:")
	for _, d := range todo {
		a.UI.Printf("  %s", driveText(d))
	}
	ok, err := a.UI.Confirm(ctx, "Einhängen und nach backup.toml suchen?")
	if err != nil || !ok {
		return false, err
	}
	done := false
	for _, d := range todo {
		mp, err := a.MountDrive(ctx, d.Device)
		if err != nil {
			a.UI.Warn("%v", err)
			continue
		}
		a.UI.Info("%s eingehängt unter %s", d.Device, mp)
		done = true
	}
	return done, nil
}

// waitForWrites flushes pending writes (the kernel buffers gigabytes for a
// slow USB stick) and shows how much is left. Ctrl+C returns early, but the
// kernel keeps writing; the process may only exit once that is done.
func (a *App) waitForWrites(ctx context.Context, title string) error {
	if a.Sync == nil {
		return nil
	}
	done := make(chan struct{})
	go func() { a.Sync(); close(done) }()
	start := time.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	announced := false
	for {
		select {
		case <-done:
			a.UI.EndProgress()
			if announced {
				a.UI.Info("%s: fertig nach %s", title, fmtDuration(time.Since(start)))
			}
			return nil
		case <-ctx.Done():
			a.UI.EndProgress()
			a.UI.Warn("Der Kernel schreibt noch Daten auf den Stick – Stick nicht abziehen, bis das Auswerfen klappt.")
			return ctx.Err()
		case <-tick.C:
			if !announced && time.Since(start) >= 2*time.Second {
				announced = true
				a.UI.Info("%s … (der Stick ist langsamer als der Rechner; bitte warten, nicht abziehen)", title)
			}
			if announced {
				a.UI.Progress(fmt.Sprintf("%s: noch %s ausstehend, %s", title, ui.Bytes(medium.PendingWrites()), fmtDuration(time.Since(start))))
			}
		}
	}
}

// driveText describes a drive for lists.
func driveText(d medium.Drive) string {
	label := d.Label
	if label == "" {
		label = "ohne Bezeichnung"
	}
	where := "nicht eingehängt"
	if d.Mounted() {
		where = d.MountPoint
		if _, f, err := medium.Space(d.MountPoint); err == nil {
			where += ", " + ui.Bytes(int64(f)) + " frei"
		}
	}
	return fmt.Sprintf("%s  %s (%s, %s) – %s", d.Device, label, d.FSType, ui.Bytes(d.Size), where)
}

func (a *App) showMediumInfo(m *Medium) {
	cfg := m.Config
	a.UI.Printf("  Name:          %s", cfg.Medium.Name)
	a.UI.Printf("  ID:            %s", cfg.Medium.ID)
	a.UI.Printf("  Pfad:          %s", m.Path)
	dev := m.Device
	if dev == "" {
		dev = "?"
	}
	if total, free, err := medium.Space(m.Path); err == nil {
		a.UI.Printf("  Gerät:         %s  (%s, %s, %s frei)", dev, m.FSType, ui.Bytes(int64(total)), ui.Bytes(int64(free)))
	} else {
		a.UI.Printf("  Gerät:         %s  (%s)", dev, m.FSType)
	}
	last := "–"
	if st, err := state.Read(m.Path); err == nil && st != nil {
		last = fmt.Sprintf("%s von %s (%s)", fmtTime(st.StartedAt), st.Host, statusText(st.Status))
	}
	a.UI.Printf("  Letzter Lauf:  %s", last)
	names := cfg.MappingNames()
	a.UI.Printf("  Mappings:      %d (%s)", len(names), strings.Join(names, ", "))
}

// validate validates the config and prints the result. It returns an error
// if the config is invalid.
func (a *App) validate(med *Medium) (*config.Result, error) {
	res := med.Config.Validate(a.Home)
	for _, w := range res.Warnings {
		a.UI.Warn("Konfiguration: %s", w)
	}
	if !res.OK() {
		return res, res.Err()
	}
	return res, nil
}

// acquireLock takes the medium lock, handling stale and foreign locks.
func (a *App) acquireLock(ctx context.Context, med *Medium, command string) (*lock.Lock, error) {
	info := lock.Info{Host: a.Host, PID: os.Getpid(), User: a.User, StartedAt: a.Now().UTC(), Command: command}
	for attempt := 0; attempt < 2; attempt++ {
		l, err := lock.Acquire(med.Path, info)
		if err == nil {
			return l, nil
		}
		var he *lock.HeldError
		if !errors.As(err, &he) {
			return nil, err
		}
		switch he.State {
		case lock.Active:
			return nil, exitf(ExitLocked, "%v", he)
		case lock.Stale:
			a.UI.Warn("veralteter Lock gefunden (Prozess läuft nicht mehr): %s", he.Info.Describe())
			ok, err := a.UI.Confirm(ctx, "Veralteten Lock entfernen?")
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, &ExitErr{Code: ExitLocked, Err: he}
			}
		case lock.Foreign:
			if !a.Opt.ForceUnlock {
				return nil, exitf(ExitLocked, "%v\nFalls dieser Lauf nicht mehr aktiv ist: --force-unlock", he)
			}
			a.UI.Warn("%v", he)
			ok, err := a.UI.Confirm(ctx, "Fremden Lock wirklich entfernen? Nur tun, wenn dort kein Backup mehr läuft!")
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, &ExitErr{Code: ExitLocked, Err: he}
			}
		}
		if err := lock.Remove(med.Path); err != nil {
			return nil, fmt.Errorf("Lock entfernen: %w", err)
		}
	}
	return nil, exitf(ExitLocked, "Lock konnte nicht gesetzt werden")
}

// showLock prints an active lock (read-only commands) and reports whether
// there is one.
func (a *App) showLock(med *Medium) bool {
	err := lock.Inspect(med.Path, a.Host)
	var he *lock.HeldError
	if !errors.As(err, &he) {
		return false
	}
	if he.State == lock.Stale {
		a.UI.Warn("veralteter Lock: %s", he.Info.Describe())
	} else {
		a.UI.Warn("%v", he)
	}
	return true
}

func statusText(s string) string {
	switch s {
	case state.StatusComplete:
		return "vollständig"
	case state.StatusRunning:
		return "unterbrochen"
	case state.StatusAborted:
		return "abgebrochen"
	case state.StatusFailed:
		return "fehlgeschlagen"
	case state.MappingOK:
		return "ok"
	case state.MappingWarning:
		return "mit Warnungen"
	case state.MappingSkipped:
		return "übersprungen"
	}
	return s
}

func fmtTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func examples(paths []string, n int) string {
	if len(paths) <= n {
		return "\n    " + strings.Join(paths, "\n    ")
	}
	return "\n    " + strings.Join(paths[:n], "\n    ") + fmt.Sprintf("\n    … und %d weitere", len(paths)-n)
}
