// Command stick-backup mirrors folders to a removable medium (typically exFAT).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/schubsijan/stick-backup/internal/app"
	"github.com/schubsijan/stick-backup/internal/ui"
)

const usage = `stick-backup – Spiegel des letzten Stands auf einem USB-Stick (exFAT)

Aufruf:
  stick-backup [Optionen] <befehl> [Argumente]

Befehle:
  init [<pfad>]        Stick einrichten (backup.toml, mirror/, logs/)
  status               Zustand des Sticks anzeigen
  run                  Spiegel aktualisieren
      --dry-run          nur prüfen und simulieren, nichts schreiben
      --only <n>[,<n>]   nur diese Mappings
      --checksum         Inhalte vergleichen statt Größe/Zeit (repariert, was verify findet)
      --force-unlock     Lock eines anderen Rechners entfernen (mit Rückfrage)
  verify               Quelle und Spiegel nach Inhalt vergleichen

Globale Optionen:
  --target <pfad>      Medium direkt angeben (keine Suche)
  -y, --yes            Bestätigungen mit „ja“ beantworten (Prüfungen laufen trotzdem)
  --pause              am Ende auf Enter warten (Aufruf aus einem Menü)
  -v, --verbose        ausführliche Ausgabe
  --no-color           keine Farben
  --version            Version anzeigen

Es gibt nur einen Stand: Was in der Quelle gelöscht wird, verschwindet beim
nächsten Lauf auch auf dem Stick. Symlinks und Dateirechte werden nicht gesichert.

Voraussetzung: ein fester Hostname (/etc/hostname), z.B. per
  sudo hostnamectl set-hostname laptop
Daran erkennt das Tool, ob der Spiegel zuletzt von einem anderen Rechner kam.

Exit-Codes: 0 Erfolg, 1 Fehler, 2 Warnungen, 3 Abbruch, 4 kein Medium, 5 Medium gesperrt
`

type flags struct {
	app.Options
	pause, noColor, help, version bool
	only                          string
}

func newFlagSet(f *flags, cmd string) *flag.FlagSet {
	fs := flag.NewFlagSet("stick-backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// Defaults are the current values: the flag set is created twice (before
	// and after the command) and must not reset earlier flags.
	fs.StringVar(&f.Target, "target", f.Target, "")
	fs.BoolVar(&f.Yes, "y", f.Yes, "")
	fs.BoolVar(&f.Yes, "yes", f.Yes, "")
	fs.BoolVar(&f.pause, "pause", f.pause, "")
	fs.BoolVar(&f.Verbose, "v", f.Verbose, "")
	fs.BoolVar(&f.Verbose, "verbose", f.Verbose, "")
	fs.BoolVar(&f.noColor, "no-color", f.noColor, "")
	fs.BoolVar(&f.help, "h", f.help, "")
	fs.BoolVar(&f.help, "help", f.help, "")
	fs.BoolVar(&f.version, "version", f.version, "")
	switch cmd {
	case "run":
		fs.BoolVar(&f.DryRun, "dry-run", false, "")
		fs.StringVar(&f.only, "only", "", "")
		fs.BoolVar(&f.Checksum, "checksum", false, "")
		fs.BoolVar(&f.ForceUnlock, "force-unlock", false, "")
	}
	return fs
}

// parse handles global flags before and after the command, and flags
// interspersed with positional arguments.
func parse(args []string) (f *flags, cmd string, pos []string, err error) {
	f = &flags{}
	// First pass: global flags up to the command.
	fs := newFlagSet(f, "")
	if err := fs.Parse(args); err != nil {
		return f, "", nil, err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return f, "", nil, nil
	}
	cmd, rest = rest[0], rest[1:]
	fs = newFlagSet(f, cmd)
	for {
		if err := fs.Parse(rest); err != nil {
			return f, cmd, nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		pos = append(pos, rest[0])
		rest = rest[1:]
	}
	if f.only != "" {
		for _, n := range strings.Split(f.only, ",") {
			if n = strings.TrimSpace(n); n != "" {
				f.Only = append(f.Only, n)
			}
		}
	}
	return f, cmd, pos, nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	f, cmd, pos, err := parse(args)
	u := ui.New(f.noColor, f.Yes, f.Verbose)
	code := dispatch(u, f, cmd, pos, err)
	if f.pause {
		if code != 0 {
			u.Printf("\nExit-Code %d", code)
		}
		u.Pause()
	}
	return code
}

func dispatch(u *ui.UI, f *flags, cmd string, pos []string, perr error) int {
	if perr != nil {
		u.Error("%v", perr)
		fmt.Fprint(os.Stderr, "\n"+usage)
		return app.ExitError
	}
	if f.version {
		u.Println("stick-backup " + app.Version)
		return 0
	}
	if f.help || cmd == "" || cmd == "help" {
		fmt.Fprint(os.Stdout, usage)
		if cmd == "" && !f.help {
			return app.ExitError
		}
		return 0
	}
	maxPos := map[string]int{"init": 1, "status": 0, "run": 0, "verify": 0}
	n, ok := maxPos[cmd]
	if !ok {
		u.Error("unbekannter Befehl %q (stick-backup --help zeigt alle Befehle)", cmd)
		return app.ExitError
	}
	if len(pos) > n {
		u.Error("%s: zu viele Argumente: %s", cmd, strings.Join(pos, " "))
		return app.ExitError
	}
	arg := ""
	if len(pos) > 0 {
		arg = pos[0]
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(u, f.Options)
	if err != nil {
		u.Error("%v", err)
		return app.ExitError
	}
	switch cmd {
	case "init":
		err = a.Init(ctx, arg)
	case "status":
		err = a.Status(ctx)
	case "run":
		err = a.Run(ctx)
	case "verify":
		err = a.Verify(ctx)
	}
	interrupted := ctx.Err() != nil // check before stop(), which cancels ctx
	stop()
	code := app.Code(err)
	if interrupted && code == app.ExitError {
		code = app.ExitAborted
	}
	var ee *app.ExitErr
	switch {
	case err == nil:
	case code == app.ExitAborted:
		u.Error("Abbruch durch den Benutzer.")
	case errors.As(err, &ee) && ee.Err == nil:
	default:
		u.Error("%v", err)
	}
	return code
}
