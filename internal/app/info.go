package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/schubsijan/stick-backup/internal/config"
	"github.com/schubsijan/stick-backup/internal/lock"
	"github.com/schubsijan/stick-backup/internal/medium"
	"github.com/schubsijan/stick-backup/internal/state"
	"github.com/schubsijan/stick-backup/internal/ui"
)

// Init implements `stick-backup init [<pfad>]`.
func (a *App) Init(ctx context.Context, path string) error {
	if a.HostErr != nil {
		a.UI.Warn("%v\nOhne festen Hostnamen funktionieren run und verify nicht.", a.HostErr)
	}
	if path == "" {
		p, err := a.chooseInitPath(ctx)
		if err != nil {
			return err
		}
		path = p
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("%s ist kein Ordner", abs)
	}
	cfgPath := filepath.Join(abs, config.FileName)
	if _, err := os.Lstat(cfgPath); err == nil {
		return fmt.Errorf("%s existiert bereits", cfgPath)
	}
	mounts, err := a.Mounts()
	if err != nil {
		return err
	}
	m, _ := medium.MountFor(mounts, abs)
	if w := medium.CheckFS(m.FSType); w != "" {
		a.UI.Warn("%s", w)
	}
	if m.MountPoint != abs {
		a.UI.Warn("%s ist kein Einhängepunkt (Einhängepunkt: %s). Die automatische Suche findet nur backup.toml im Wurzelverzeichnis eines Dateisystems; sonst --target verwenden.", abs, m.MountPoint)
	}
	def := filepath.Base(abs)
	if def == "/" || def == "." {
		def = "Backup"
	}
	name, err := a.UI.Ask(ctx, "Name des Mediums", def)
	if err != nil {
		return err
	}
	id, err := config.NewUUID()
	if err != nil {
		return err
	}
	for _, d := range []string{"mirror", "logs"} {
		if err := os.MkdirAll(filepath.Join(abs, d), 0o755); err != nil {
			if errors.Is(err, os.ErrPermission) {
				return fmt.Errorf("%w\nKeine Schreibrechte. Ein frisch formatiertes ext4 gehört root, übernehmen mit: sudo chown %s: %s", err, a.User, abs)
			}
			return err
		}
	}
	f, err := os.OpenFile(cfgPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(config.Template(id, name)); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	a.UI.Success("Medium %q eingerichtet: %s", name, abs)
	a.UI.Hint("Gesichert wird ein Spiegel des letzten Stands (Ordner mirror/), keine älteren Versionen.")
	a.UI.Hint("Symlinks und Dateirechte werden nicht gesichert; der Stick ist nicht verschlüsselt.")
	a.UI.Hint("Jetzt die Mappings in %s eintragen.", cfgPath)
	return nil
}

// chooseInitPath offers mounted removable drives without backup.toml, plus
// the current directory. With --yes, or when no drive is found, it asks for
// the current directory only.
func (a *App) chooseInitPath(ctx context.Context) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	var drives []medium.Drive
	if !a.UI.Yes && a.Drives != nil {
		for _, d := range a.Drives(ctx) {
			if d.Mounted() {
				if _, err := os.Lstat(filepath.Join(d.MountPoint, config.FileName)); err == nil {
					continue // already a medium
				}
			}
			drives = append(drives, d)
		}
	}
	if len(drives) == 0 {
		if !a.UI.Yes {
			a.UI.Info("Kein Wechseldatenträger ohne backup.toml gefunden (Stick eingesteckt?).")
		}
		ok, err := a.UI.Confirm(ctx, fmt.Sprintf("Aktuelles Verzeichnis %s als Backup-Medium einrichten?", wd))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ui.ErrAborted
		}
		return wd, nil
	}
	a.UI.Heading("Wechseldatenträger:")
	for i, d := range drives {
		a.UI.Printf("  [%d] %s", i+1, driveText(d))
	}
	a.UI.Printf("  [%d] aktuelles Verzeichnis %s", len(drives)+1, wd)
	i, err := a.UI.Choose(ctx, "Wo soll das Medium eingerichtet werden?", len(drives)+1)
	if err != nil {
		return "", err
	}
	if i == len(drives) {
		return wd, nil
	}
	d := drives[i]
	if d.Mounted() {
		return d.MountPoint, nil
	}
	mp, err := a.MountDrive(ctx, d.Device)
	if err != nil {
		return "", err
	}
	a.UI.Info("%s eingehängt unter %s", d.Device, mp)
	if _, err := os.Lstat(filepath.Join(mp, config.FileName)); err == nil {
		return "", fmt.Errorf("%s enthält bereits eine backup.toml – der Stick ist schon eingerichtet (stick-backup status)", mp)
	}
	return mp, nil
}

// Status implements `stick-backup status`.
func (a *App) Status(ctx context.Context) error {
	med, err := a.selectMedium(ctx)
	if err != nil {
		return err
	}
	warnings := 0
	if a.HostErr != nil {
		a.UI.Warn("%v", a.HostErr)
		warnings++
	}
	a.UI.Println("")
	a.UI.Heading("Medium")
	a.UI.Printf("  Name:  %s", med.Config.Medium.Name)
	a.UI.Printf("  ID:    %s", med.Config.Medium.ID)
	a.UI.Printf("  Pfad:  %s (%s)", med.Path, med.FSType)
	if total, free, err := medium.Space(med.Path); err == nil {
		a.UI.Printf("  Platz: %s frei von %s", ui.Bytes(int64(free)), ui.Bytes(int64(total)))
	}

	a.UI.Println("")
	a.UI.Heading("Konfiguration")
	res := med.Config.Validate(a.Home)
	if res.OK() {
		a.UI.Success("  gültig, %d Mappings", len(med.Config.Mappings))
	}
	for _, e := range res.Errors {
		a.UI.Error("%s", e)
	}
	for _, w := range res.Warnings {
		a.UI.Warn("%s", w)
	}
	warnings += len(res.Errors) + len(res.Warnings)

	a.UI.Println("")
	a.UI.Heading("Lock")
	if _, err := os.Lstat(lock.Path(med.Path)); errors.Is(err, os.ErrNotExist) {
		a.UI.Printf("  kein aktiver Lauf")
	} else if a.showLock(med) {
		warnings++
	}

	a.UI.Println("")
	a.UI.Heading("Spiegel")
	st, err := state.Read(med.Path)
	switch {
	case err != nil:
		a.UI.Warn("Statusdatei unlesbar: %v", err)
		warnings++
	case st == nil:
		a.UI.Printf("  noch kein Lauf")
	default:
		a.UI.Printf("  Letzter Lauf: %s von %s, %s", fmtTime(st.StartedAt), st.Host, statusText(st.Status))
		if st.Status != state.StatusComplete {
			a.UI.Warn("der letzte Lauf wurde nicht abgeschlossen – der Spiegel ist nur teilweise aktuell; der nächste Lauf bringt ihn wieder auf Stand")
			warnings++
		}
		for _, m := range med.Config.Mappings {
			ms := st.Mapping(m.CleanTarget())
			if ms == nil || ms.UpdatedAt == nil {
				a.UI.Printf("  %-16s noch nicht gesichert", m.Name)
				continue
			}
			a.UI.Printf("  %-16s Stand %s von %s, %s, %d Dateien", m.Name, fmtTime(*ms.UpdatedAt), ms.Host, statusText(ms.Status), ms.FilesTotal)
		}
	}
	if warnings > 0 {
		return &ExitErr{Code: ExitWarnings}
	}
	return nil
}
