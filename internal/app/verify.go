package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/schubsijan/stick-backup/internal/config"
	"github.com/schubsijan/stick-backup/internal/gitfilter"
	"github.com/schubsijan/stick-backup/internal/rsync"
	"github.com/schubsijan/stick-backup/internal/state"
)

// Verify implements `stick-backup verify`: compare source and mirror by content.
func (a *App) Verify(ctx context.Context) error {
	if err := a.requireHost(); err != nil {
		return err
	}
	med, err := a.selectMedium(ctx)
	if err != nil {
		return err
	}
	if _, err := a.validate(med); err != nil {
		return err
	}
	if _, _, err := rsync.Version(ctx); err != nil {
		return err
	}
	a.showLock(med)
	st, err := state.Read(med.Path)
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("auf dem Medium gibt es noch keinen Spiegel")
	}
	if st.Status != state.StatusComplete {
		a.UI.Warn("der letzte Lauf wurde nicht abgeschlossen (%s), Abweichungen sind zu erwarten", statusText(st.Status))
	}
	a.UI.Heading("Vergleiche Quelle und Spiegel nach Inhalt")
	a.UI.Hint("Alle Dateien werden vollständig gelesen (Quelle und Stick). Das kann bei großen Datenmengen lange dauern.")

	needGit := false
	for _, m := range med.Config.Mappings {
		needGit = needGit || m.IsGit()
	}
	if needGit {
		if _, err := gitfilter.Version(ctx); err != nil {
			return err
		}
	}
	tmp, err := gitfilter.TempDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	suspicious := 0
	for i := range med.Config.Mappings {
		m := &med.Config.Mappings[i]
		target := m.CleanTarget()
		ms := st.Mapping(target)
		if ms == nil || ms.UpdatedAt == nil {
			a.UI.Info("%s: noch nicht im Spiegel, übersprungen", m.Name)
			continue
		}
		if ms.Host != a.Host {
			a.UI.Warn("%s wurde zuletzt von %s gesichert – Unterschiede zu diesem Rechner sind zu erwarten", m.Name, ms.Host)
		}
		src, err := config.ExpandSource(m.Source, a.Home)
		if err == nil {
			src, err = filepath.EvalSymlinks(src)
		}
		if err != nil {
			a.UI.Warn("%s: Quelle fehlt: %v", m.Name, err)
			suspicious++
			continue
		}
		j := newJob(med.Config, m, src, med.MirrorDir)
		if m.IsGit() {
			dir := filepath.Join(tmp, fmt.Sprintf("m%d", i))
			if err := os.Mkdir(dir, 0o700); err != nil {
				return err
			}
			res, err := gitfilter.BuildList(ctx, gitfilter.Options{Root: src, Excludes: j.excludes, Includes: m.Include, GitDirs: m.GitDirs(), TmpDir: dir})
			if err != nil {
				return fmt.Errorf("%s: Dateiliste: %w", m.Name, err)
			}
			j.files = res.Files
			j.listPath = filepath.Join(dir, "files")
			if err := gitfilter.WriteList(j.listPath, j.files); err != nil {
				return err
			}
		}
		// Names differing only in case share one file on exFAT; their
		// content difference is no bitrot.
		paths, err := j.selectedPaths(ctx)
		if err != nil {
			return fmt.Errorf("%s: %w", m.Name, err)
		}
		collide := map[string]bool{}
		groups := caseCollisions(paths)
		for _, g := range groups {
			for _, p := range g {
				collide[p] = true
			}
		}
		if len(groups) > 0 {
			a.UI.Warn("%s: %d Namen unterscheiden sich nur in Groß-/Kleinschreibung und teilen sich auf dem Stick eine Datei – nicht prüfbar", m.Name, len(groups))
			suspicious++
		}

		o := j.opts(src, j.dst)
		o.DryRun, o.Checksum = true, true
		var bad []string
		changed := 0
		a.UI.Info("%s …", m.Name)
		res, err := rsync.Run(ctx, rsync.Args(o), rsync.Handlers{Item: func(it rsync.Item) {
			switch {
			case it.IsDir(), collide[it.Path]:
			case it.ContentOnlyDiff():
				bad = append(bad, it.Path)
			default:
				changed++
			}
		}})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 && !res.ExitWarning() {
			return fmt.Errorf("%s: rsync-Code %d: %s", m.Name, res.ExitCode, strings.Join(res.Stderr, "; "))
		}
		if res.ExitWarning() {
			a.UI.Warn("%s: nicht alle Dateien lesbar (rsync-Code %d)", m.Name, res.ExitCode)
			suspicious++
		}
		if len(bad) > 0 {
			suspicious += len(bad)
			a.UI.Error("%s: %d Dateien unterscheiden sich im Inhalt, obwohl Größe und Änderungszeit gleich sind (Datenfehler auf dem Stick oder manipulierte Änderungszeit):%s", m.Name, len(bad), examples(bad, 20))
		}
		a.UI.Printf("  %s: %d auffällig, %d seit dem letzten Lauf normal geändert", m.Name, len(bad), changed)
	}
	if suspicious > 0 {
		a.UI.Hint("Auffällige Dateien neu schreiben: stick-backup run --checksum")
		return &ExitErr{Code: ExitWarnings}
	}
	a.UI.Success("Keine Auffälligkeiten.")
	return nil
}
