# Spezifikation: stick-backup – Spiegel auf einem USB-Stick (exFAT)

Status: Entwurf · Stand: 2026-11-17 · Tool-Version 0.2.0 · `format_version` der Konfiguration: 1

Name des Programms: `stick-backup`. Die Dateien auf dem Stick (`backup.toml`, `.backup-state.json`,
`.backup.lock`) behalten ihre Namen.

Die frühere Fassung mit Snapshots, Hardlinks und LUKS (Stand 2026-11-17, vor der Reduktion)
liegt samt Code in `archiv/stand-mit-snapshots-2026-11-17.tar.gz`.

---

## 1. Ziel

Ein eigenständiges CLI-Tool in Go, das festgelegte Ordner eines Rechners auf einen USB-Stick
sichert, typischerweise mit **exFAT** (dem Werksformat großer Sticks, auch unter Windows und
macOS lesbar). **Was** wohin gesichert wird, steht in einer Konfigurationsdatei **auf dem
Stick**. Mehrere Rechner können denselben Stick mit derselben Konfiguration verwenden.

Auf dem Stick liegt **genau ein Spiegel des letzten Stands**: ein normaler Ordnerbaum, den man
mit jedem Dateimanager lesen kann. Es gibt **keine älteren Versionen**: Was in der Quelle
gelöscht oder geändert wird, ist nach dem nächsten Lauf auch auf dem Stick gelöscht bzw.
geändert.

### Nicht-Ziele (Version 1)

- keine Versionen/Snapshots (exFAT kennt keine Hardlinks; Vollkopien würden den Stick füllen)
- keine eigene Kopier-Engine: **rsync** erledigt das Kopieren
- keine Verschlüsselung: Wer den Stick hat, kann alles lesen
- keine Dateirechte, Eigentümer und Symlinks (exFAT kann sie nicht speichern)
- kein GUI; Aufruf aus einem Menü über ein Terminal (z.B. `kitty -e stick-backup run --pause`)
- kein automatischer Start beim Einstecken (eingehängt wird nur nach Rückfrage, siehe §5)
- kein `restore`-Befehl (Wiederherstellen = Ordner vom Stick kopieren, siehe §10)
- kein Git als Speicher; Git dient nur optional zur *Dateiauswahl* (§7)
- keine Synchronisierung *zwischen* Rechnern

### Was exFAT nicht kann (mit einem exFAT-Image geprüft)

| Eigenschaft | Folge für das Tool |
|---|---|
| keine Hardlinks | keine Snapshots, nur ein Spiegel |
| keine Symlinks | Symlinks werden übersprungen (Hinweis mit Anzahl und Beispielen) |
| keine Unix-Rechte, kein Eigentümer | alles erscheint als `rwxr-xr-x` des einhängenden Benutzers |
| Zeitstempel auf 10 ms (FAT: 2 s) | Vergleich mit 1 s Toleranz (`--modify-window=1`) |
| Groß-/Kleinschreibung egal | `Readme` und `README` sind dieselbe Datei; das Tool warnt vorher (§6.3) |
| Zeichen `" * : < > ? \ \|` verboten | rsync-Code 23 → Warnung mit Erklärung |
| kein Journal | vor dem Abziehen auswerfen; `run` erinnert daran |

Das Tool läuft auch auf anderen Dateisystemen (ext4, NTFS, …), verhält sich dort aber genauso
(keine Rechte, keine Symlinks). Bei FAT32 (`vfat`) warnt es zusätzlich: Dateien über 4 GB
lassen sich nicht speichern.

---

## 2. Begriffe

| Begriff | Bedeutung |
|---|---|
| **Medium** | Eingehängtes Dateisystem mit einer `backup.toml` im Wurzelverzeichnis |
| **Mapping** | Ein Paar aus Quellordner (Rechner) und Zielordner (relativ im Spiegel) |
| **Spiegel** | Ordner `mirror/` auf dem Medium mit dem letzten Stand aller Mappings |
| **Host** | Rechner, ermittelt über den **festen** Hostnamen (§3) |

---

## 3. Aufbau des Mediums

```
<medium>/
  backup.toml              Konfiguration (vom Benutzer gepflegt)
  .backup-state.json       Status des Spiegels (§9)
  .backup.lock             existiert nur während eines Laufs
  mirror/
    Dokumente/Privat/Programmieren/...
    Spiele/Minecraft/saves/...
  logs/
    2026-11-17_204608_schubsis-laptop.log
```

Reserviert sind `backup.toml`, `mirror`, `logs` und alles, was mit `.backup` beginnt.
Mapping-Ziele liegen *innerhalb* von `mirror/` und kollidieren nicht damit.

### Host-Name

Der Host wird aus `/etc/hostname` gelesen (erste Zeile, die nicht leer und kein Kommentar ist),
also dem festen Namen, den `hostnamectl set-hostname` setzt. `os.Hostname()` wird **nicht**
verwendet: Ist kein fester Name gesetzt, liefert es den flüchtigen Namen, den z.B. DHCP/
Reverse-DNS vergibt (beobachtet: `2a02-2455-….dyn6.pyur.net`). Der ändert sich mit dem
Netzwerk, und das Tool könnte nicht mehr erkennen, welcher Rechner den Spiegel zuletzt
geschrieben hat (§6.2 a).

Ist `/etc/hostname` leer oder fehlt, brechen `run` und `verify` mit Exit-Code `1` und dem
Hinweis `sudo hostnamectl set-hostname <name>` ab; `init` und `status` warnen; `--help` nennt
die Voraussetzung.

---

## 4. Konfiguration (`backup.toml`)

### Beispiel

```toml
format_version = 1

[medium]
id   = "6f1c2e9a-8b4d-4f3a-9c51-2d7e0b8a4f11"   # von `stick-backup init` erzeugt, nicht ändern
name = "INTENSO"                                # frei wählbar, nur zur Anzeige

[safety]
max_delete_percent = 20   # mehr Löschungen gegenüber dem Spiegel → Bestätigung nötig
allow_empty_source = false

[defaults]
exclude = ["*.tmp", "*.swp", "**/.cache/"]

[[mapping]]
name    = "Programmieren"
source  = "~/Documents/Privat/Programmieren/"
target  = "Dokumente/Privat/Programmieren/"
exclude = ["**/node_modules/", "**/target/", "**/__pycache__/", "*.o"]

[[mapping]]
name   = "Minecraft"
source = "~/.minecraft/saves/"
target = "Spiele/Minecraft/saves/"
use_default_excludes = false   # Welten vollständig sichern
```

### Felder

| Feld | Typ | Pflicht | Standard | Bedeutung |
|---|---|---|---|---|
| `format_version` | int | ja | – | Version des Formats, aktuell `1` |
| `medium.id` | string (UUID) | ja | – | Eindeutige ID des Mediums |
| `medium.name` | string | ja | – | Anzeigename |
| `safety.max_delete_percent` | int 0–100 | nein | `20` | siehe §6.2 b |
| `safety.allow_empty_source` | bool | nein | `false` | leere Quelle erlauben |
| `defaults.exclude` | []string | nein | `[]` | Muster für alle Mappings |
| `mapping[].name` | string | ja | – | eindeutig, für Anzeige, Log und `--only` |
| `mapping[].source` | string | ja | – | absoluter Pfad oder mit `~/` beginnend |
| `mapping[].target` | string | ja | – | relativer Pfad in `mirror/` |
| `mapping[].exclude` | []string | nein | `[]` | zusätzliche Muster |
| `mapping[].use_default_excludes` | bool | nein | `true` | `defaults.exclude` anwenden |
| `mapping[].filter` | `"rsync"` \| `"git"` | nein | `"rsync"` | Art der Dateiauswahl, siehe §7 |
| `mapping[].include` | []string | nein | `[]` | `rsync`: **nur** diese Pfade sichern (Positivliste, siehe unten); `git`: trotz Ignore-Regeln sichern |
| `mapping[].backup_git_dirs` | bool | nein | `true` | nur bei `filter = "git"`: `.git` der Repos mitsichern |

Unbekannte Felder führen zu einem **Fehler** (Tippfehler sollen auffallen). Das gilt auch für
`[retention]` und `medium.mode` aus der früheren Fassung.

### Pfade

- `source`: `~` bzw. `~/` wird zum Home-Verzeichnis des **aufrufenden Benutzers** aufgelöst.
  Danach muss der Pfad absolut sein. Symlinks im Quellpfad selbst werden aufgelöst
  (`filepath.EvalSymlinks`), innerhalb der Quelle nicht.
- `target`: muss relativ sein, wird mit `filepath.Clean` normalisiert und darf danach weder
  leer noch `.` sein und kein `..`-Segment enthalten.

### Validierung (vor jedem Lauf, auch bei `--dry-run`)

1. `format_version` bekannt. Ist sie **größer** als die unterstützte → Abbruch „Tool zu alt“.
2. `medium.id` ist eine gültige UUID, `medium.name` nicht leer.
3. Mapping-Namen eindeutig (Groß-/Kleinschreibung ignoriert).
4. Targets gültig; **keine überlappenden Targets** (gleich oder pfadweise Präfix).
5. Überlappende Sources → **Warnung**, kein Fehler.
6. Exclude-/Include-Muster nicht leer.
7. `filter = "rsync"`: keine Negationen (`!`) in `exclude` und `include`, kein `..` in `include`,
   kein `backup_git_dirs`.
   Negationen in `defaults.exclude` nur, wenn alle Mappings, die sie verwenden, Git-Filter haben.
8. `safety.max_delete_percent` zwischen 0 und 100.

### Positivliste (`include` bei `filter = "rsync"`)

Ist `include` gesetzt, sichert das Tool **nur** die aufgeführten Pfade. Neue Ordner in der
Quelle (z.B. ein neuer Cache) landen so nicht ungewollt auf dem Stick.

```toml
[[mapping]]
name    = "Minecraft"
source  = "~/.local/share/PrismLauncher/instances/Mein Client Mods/"
target  = "Minecraft/Mein Client Mods/"
include = [
  "instance.cfg",              # Datei
  "minecraft/saves/Chill/",    # Ordner mit allem darunter
  "minecraft/options.txt",
]
exclude = ["*.tmp"]            # gilt auch innerhalb der Positivliste
```

- Pfade relativ zur Quelle, führendes `/` optional, rsync-Platzhalter erlaubt.
- Umsetzung (`rsync.IncludeRules`): jeder Eltern-Ordner als eigene Regel (`--include=/minecraft/`,
  `--include=/minecraft/saves/`), der Pfad als Datei (`--include=/p`) und als Ordner mit Inhalt
  (`--include=/p/***`); ein `/` am Ende beschränkt auf Ordner. Danach `--exclude=*`.
- Reihenfolge: zuerst alle `--exclude`, dann die Includes, dann `--exclude=*` (bei rsync gewinnt
  die erste passende Regel). Mit rsync 3.5.2 geprüft.
- Wird ein Pfad aus der Liste entfernt, löscht `--delete-excluded` ihn beim nächsten Lauf vom Stick.
- Pfade ohne Platzhalter, die es in der Quelle nicht gibt, ergeben eine **Warnung**
  („Tippfehler?“), weil sie sonst still fehlen würden.

### Exclude-Muster

- **`filter = "rsync"`** (Standard): unverändert als `--exclude=<muster>` an rsync (rsync-Syntax).
- **`filter = "git"`**: gitignore-Syntax inklusive Negationen (§7).

| Muster | Wirkung |
|---|---|
| `*.tmp` | Dateien mit dieser Endung in jeder Tiefe |
| `build/` | jeder Ordner namens `build` |
| `/build/` | nur `build` direkt im Quellordner |
| `**/x/` | Ordner `x` in beliebiger Tiefe |

---

## 5. Erkennung des Mediums und Befehle

### Medium finden

Ohne `--target` liest das Tool `/proc/self/mountinfo`, überspringt Pseudo-Dateisysteme,
`/`, `/boot`, `/boot/efi`, `/proc`, `/sys`, `/dev`, und nimmt jeden Einhängepunkt mit einer
lesbaren, gültigen `backup.toml` (Bind-Mounts bzw. gleiche `medium.id` nur einmal; ungültige
Dateien → Warnung mit Pfad).

Nicht jede Umgebung hängt Sticks automatisch ein (beobachtet: Stick eingesteckt, aber nicht
eingehängt). Gibt es keinen Kandidaten, sucht das Tool deshalb **nicht eingehängte
Wechseldatenträger** (lsblk wie bei `init`), zeigt sie an und fragt „Einhängen und nach
backup.toml suchen?“. Bei „ja“ (oder `--yes`) hängt es sie mit
`udisksctl mount -b <gerät> --no-user-interaction` ein (ohne sudo; der Einhängepunkt kommt aus
der Ausgabe „Mounted … at …“, mit `LC_ALL=C`) und sucht erneut. Fehlt die Berechtigung, nennt
die Meldung den Dateimanager als Ausweg.

| Ergebnis | Verhalten |
|---|---|
| 0 Kandidaten | Meldung mit Hinweis auf `stick-backup init`, Exit-Code `4` |
| 1 Kandidat | Infos (Name, ID, Pfad, Gerät, Größe/frei, letzter Lauf, Mappings), Bestätigung `[j/N]` |
| ≥ 2 Kandidaten | Nummerierte Auswahl; mit `--yes` → Fehler, Exit-Code `1` |

`--yes` überspringt die Bestätigung nur bei genau einem Kandidaten.

### Globale Optionen

| Option | Bedeutung |
|---|---|
| `--target <pfad>` | Medium direkt angeben (keine Suche) |
| `-y`, `--yes` | Bestätigungen beantworten; **Prüfungen laufen trotzdem** (§6.2) |
| `--pause` | Am Ende „Enter zum Schließen“ (auch bei Fehlern und Abbruch) |
| `-v`, `--verbose` | ausführliche Ausgabe |
| `--no-color` | keine Farben (automatisch ohne Terminal oder mit `NO_COLOR`) |
| `--version`, `-h` | Version, Hilfe |

Optionen dürfen vor oder nach dem Befehl stehen.

### `stick-backup init [<pfad>]`

Legt `backup.toml` (neue UUID, abgefragter Name, kommentierte Beispiel-Mappings), `mirror/`
und `logs/` an. Bricht ab, wenn `backup.toml` existiert.

Ohne `<pfad>` bietet das Tool alle **Wechseldatenträger** ohne `backup.toml` als
nummerierte Auswahl an, auch nicht eingehängte (sie werden bei Auswahl wie oben eingehängt) (lsblk `--json --bytes -o NAME,PATH,FSTYPE,MOUNTPOINTS,SIZE,LABEL,HOTPLUG`
mit `LC_ALL=C`; `HOTPLUG` wird von übergeordneten Geräten geerbt, interne Platten erscheinen
nicht). Angezeigt werden Gerät, Bezeichnung, Dateisystem, Größe und Einhängepunkt mit freiem
Platz bzw. „nicht eingehängt“; als letzter
Eintrag das aktuelle Verzeichnis. Ohne Datenträger oder mit `--yes` → aktuelles Verzeichnis
nach Bestätigung. Fehlen Schreibrechte, nennt die Meldung `sudo chown <benutzer>: <pfad>`.
`init` weist auf die Einschränkungen hin (kein Versionsverlauf, keine Rechte/Symlinks, keine
Verschlüsselung).

### `stick-backup status`

Medium, Konfigurations-Validierung, Lock, letzter Lauf (Zeit, Host, Status) und pro Mapping:
Stand, Host des letzten Laufs, Status, Anzahl Dateien. Warnt, wenn der letzte Lauf nicht
abgeschlossen wurde. Ändert nichts.

### `stick-backup run [--dry-run] [--only <name>[,<name>…]] [--force-unlock]`

Hauptbefehl, Ablauf in §6.

- `--dry-run`: alle Prüfungen und eine rsync-Simulation; schreibt nichts aufs Medium (auch
  keinen Lock, kein Log, keine Statusdatei), zeigt pro Mapping, wie viele Dateien übertragen
  und gelöscht würden.
- `--only`: nur die genannten Mappings; die übrigen bleiben im Spiegel unverändert.
- `--checksum`: Simulation und Kopie vergleichen den **Inhalt** (`rsync --checksum`) statt
  Größe und Zeit; liest alles auf beiden Seiten. Repariert Dateien, die `verify` als auffällig
  meldet. Beobachtet: Nach dem Abziehen während ausstehender Schreibvorgänge hatten 21 Dateien
  auf dem Stick richtige Größe und Zeit, aber ab einer MiB-Grenze fremden Inhalt – ein normaler
  Lauf erkennt das nicht.
- `--force-unlock`: Lock eines anderen Rechners nach Bestätigung entfernen (§8).

### `stick-backup verify`

Vergleicht Quelle und Spiegel **nach Inhalt** (`rsync -rtnc --modify-window=1
--itemize-changes`, bei Git-Mappings mit neu erstellter Dateiliste). Ausgabe pro Mapping:

- Dateien, deren Inhalt sich unterscheidet, obwohl Größe und Zeit gleich sind → **auffällig**
  (Datenfehler auf dem Stick oder manipulierte Zeit)
- sonstige Änderungen seit dem letzten Lauf → nur Anzahl
- Namen, die sich nur in der Schreibung unterscheiden, gelten nicht als auffällig, sondern
  werden als „nicht prüfbar“ gemeldet (sie teilen sich auf exFAT eine Datei).
- Mappings, die zuletzt ein anderer Rechner gesichert hat → Warnung, Unterschiede erwartbar.

Ändert nichts. Kann lange dauern; das wird vor dem Start angezeigt. Bei Auffälligkeiten
nennt es `stick-backup run --checksum` zum Reparieren.

### Exit-Codes

| Code | Bedeutung |
|---|---|
| `0` | Erfolg |
| `1` | Fehler (Konfiguration, rsync, Ein-/Ausgabe, …) |
| `2` | Erfolg mit Warnungen (bei `verify`: Auffälligkeiten) |
| `3` | Abbruch durch den Benutzer (Bestätigung verneint, Strg+C) |
| `4` | kein Medium gefunden |
| `5` | Medium gesperrt (Lock eines anderen Laufs aktiv) |

---

## 6. Ablauf von `stick-backup run`

### 6.1 Vorbereitung

1. Fester Hostname vorhanden (§3).
2. Medium bestimmen (§5), Konfiguration laden und validieren (§4).
3. `rsync` ≥ 3.1.0 (`--info=progress2`). Bei Git-Mappings `git` ≥ 2.32 (`GIT_CONFIG_GLOBAL`).
4. Außer bei `--dry-run`: `mirror/` und `logs/` anlegen, Lock setzen (§8), Log öffnen. Ab hier
   wird der Lock auch bei Fehlern/Strg+C wieder entfernt.
5. Stehen noch mehr als 32 MB Schreibdaten aus (z.B. von einem früheren Vorgang), wartet das
   Tool sichtbar darauf (§6.7), bevor es prüft – sonst würden alle Prüfungen still dahinter warten.
6. Statusdatei lesen (§9). War der letzte Lauf nicht abgeschlossen → Hinweis.
7. Pro Mapping die Quelle prüfen: existiert, ist ein Ordner, lesbar, nicht leer (außer
   `allow_empty_source`). Sonst Abbruch des gesamten Laufs: ein Spiegel mit fehlender Quelle
   würde alle gesicherten Dateien des Mappings löschen.
8. Git-Mappings: Dateiliste erstellen (§7).
9. Meldung „Prüfe Quellen und vergleiche mit dem Stick …“ vor den folgenden Prüfungen.

### 6.2 Sicherheitsprüfungen (vor dem Schreiben)

Nur für Mappings, die schon im Spiegel liegen.

**a) Zuletzt von einem anderen Rechner gesichert**
Steht in der Statusdatei für das Mapping ein anderer Host:

```
rsync -rtn --modify-window=1 --update --itemize-changes <excludes> mirror/<target>/ <source>/
```

Jede Datei, die übertragen würde, ist im Spiegel neuer als hier oder fehlt hier. Bei > 0 →
Warnung „*Saves* wurde zuletzt von *desktop* gesichert (2026-11-12 20:10). Im Spiegel sind 37
Dateien neuer als hier oder fehlen hier – beim Sichern werden sie überschrieben bzw. gelöscht“,
Beispiele, **Bestätigung nötig**. Bei Git-Mappings in Go: Dateien im Spiegel durchlaufen, zählen
wenn lokal fehlend oder im Spiegel mehr als 1 s neuer.

Da es nur einen Spiegel gibt, ist das die wichtigste Prüfung bei mehreren Rechnern: Ohne sie
würde ein Rechner mit älterem Stand die neueren Dateien des anderen löschen.

**b) Viele Löschungen**

```
rsync -rtn --modify-window=1 --delete --delete-excluded --itemize-changes <excludes> <source>/ mirror/<target>/
```

Gezählt werden `*deleting`-Zeilen (nur Dateien) im Verhältnis zur Anzahl Dateien im Spiegel
(aus der Statusdatei, sonst per Verzeichnisdurchlauf). Übersteigt der Anteil
`max_delete_percent` → Warnung mit Anzahl und bis zu 10 Pfaden, **Bestätigung nötig**.
Git-Mappings: Dateien im Spiegel minus aktuelle Dateiliste, in Go.

**c) Freier Platz**
Aus der Simulation (`--stats --no-human-readable`, „Total transferred file size“, Summe über
alle Mappings) plus 5 % Reserve. Reicht der Platz nicht → Abbruch.

**`--yes`** beantwortet die Bestätigungen aus a) und b) mit „ja“, gibt die Warnungen aber aus
und protokolliert sie. c) und alle Fehler aus §6.1 lassen sich nicht übergehen.

### 6.3 Groß-/Kleinschreibung

Auf exFAT sind `Readme` und `README` dieselbe Datei: eine überschreibt die andere, **und
rsync bemerkt das auch beim nächsten Lauf nicht** (geprüft; beide Quelldateien erscheinen
gleich groß und gleich alt wie die eine Datei auf dem Stick). Vor dem Kopieren listet das Tool
deshalb alle ausgewählten Pfade (`rsync -r --list-only <excludes>`, bei Git-Mappings die
Dateiliste samt Ordnern) und warnt bei Namen, die sich nur in der Schreibung unterscheiden
(keine Bestätigung nötig, aber Exit-Code `2`).

### 6.4 Spiegel schreiben

1. Statusdatei mit `status = "running"` schreiben; Einträge nicht laufender Mappings
   (`--only`) werden übernommen.
2. Pro Mapping nacheinander:

   ```
   rsync -rt --modify-window=1 --delete --delete-excluded --fsync \
         --exclude=<muster> … \
         --itemize-changes --out-format='%i %n%L' --stats --no-human-readable \
         --info=progress2 \
         <source>/ mirror/<target>/
   ```

   - Übergeordnete Ordner von `mirror/<target>` legt das Tool an (rsync legt nur die letzte
     Ebene an).
   - `-rt` statt `-a`: keine Rechte, Eigentümer, Gruppen, Geräte; Symlinks werden übersprungen
     (`skipping non-regular file "…"` → Hinweis, keine Warnung, sonst endete jeder Lauf mit
     Code `2`).
   - `--delete-excluded`: Was neu ausgeschlossen wird, verschwindet auch vom Stick.
   - `--modify-window=1`: exFAT speichert Zeiten nur auf 10 ms; mit genauerem Vergleich
     erschiene jede Datei bei jedem Lauf geändert. Folge: Eine Änderung innerhalb derselben
     Sekunde bei gleicher Größe wird nicht erkannt.
   - `--fsync` (rsync ≥ 3.2.4, sonst weggelassen): jede Datei ist auf dem Stick, bevor die
     nächste beginnt. Ohne die Option nimmt Linux Gigabytes in den Arbeitsspeicher auf und
     schreibt sie im Hintergrund (beobachtet: 2 GB nach einem „erfolgreichen“ Lauf noch nicht auf
     dem Stick). Folgen waren eine springende Fortschrittsanzeige und minutenlange stille Pausen,
     weil spätere Zugriffe auf den Stick hinter dem Rückstau warten.
   - Fortschritt auf dem Terminal, `--out-format`-Zeilen ins Log.
   - Alle rsync-Aufrufe mit `LC_ALL=C`; `--stats` ist sonst sprachabhängig formatiert
     (`60.000.008 bytes`). Der Parser ignoriert Tausendertrennzeichen zusätzlich.
3. rsync-Exit-Codes:
   - `0` → ok
   - `24` (Dateien während der Übertragung verschwunden) → **Warnung**
   - `23` (teilweise Übertragung) → **Warnung** mit Pfaden aus stderr; bei „Invalid argument“
     Hinweis auf unzulässige Zeichen, bei „File too large“ auf die 4-GB-Grenze von FAT32
   - alles andere → **Fehler**, Lauf bricht ab
4. Git-Mappings: `--files-from` kann nicht löschen; das Tool entfernt danach im Spiegel alle
   Dateien, die nicht in der Liste stehen, und leere Ordner.
5. **Nachkontrolle** (`rsync -rtn --modify-window=1 --itemize-changes …`): jede Zeile außer
   reinen Ordner-Zeitstempeln bedeutet, dass sich die Quelle während des Kopierens geändert
   hat → **Warnung** mit Pfaden. Dateien, die rsync schon mit Code 23 gemeldet hat, werden
   ausgelassen (auch über den temporären Namen `.name.XXXXXX` in der Meldung).
6. Statusdatei nach jedem Mapping aktualisieren: Host, Zeit, Status, Zahlen, Warnungen.

### 6.5 Abschluss

1. Warten, bis alle Daten auf dem Stick sind (§6.7). Erst danach gilt der Lauf als erfolgreich.
   Statusdatei: `status = "complete"`, Endzeit; `fsync`.
2. Logs älter als 90 Tage löschen.
3. Zusammenfassung pro Mapping.
4. Log abschließen, Lock entfernen, danach **nochmals alles festschreiben** (§6.7): Das
   Entfernen des Locks ändert den Ordner auf dem Stick; ohne diesen Schritt ging beim Abziehen
   kurz nach dem Lauf ein Schreibvorgang verloren (beobachtet: `lost async page write`, Volume
   danach „not properly unmounted“).
5. Ist das Medium ein eingehängter Wechseldatenträger (lsblk `HOTPLUG`, Gerät und
   Einhängepunkt stimmen überein), fragt das Tool „Stick jetzt auswerfen? [J/n]“ und hängt ihn
   mit `udisksctl unmount -b <gerät>` aus („kannst ihn jetzt abziehen“). Bei „n“ oder Fehler
   (z.B. Stick noch in Benutzung) → Hinweis, vor dem Abziehen auszuwerfen. Bei internen Platten
   und `--target`-Ordnern wird nie gefragt.
6. Exit-Code `0`, bei Warnungen `2`.

### 6.6 Abbruch (Strg+C, SIGTERM) und Fehler

- Laufendes rsync erhält SIGTERM.
- Statusdatei: `status = "aborted"` bzw. `"failed"`; das betroffene Mapping `failed`, noch
  nicht begonnene `skipped`.
- Der Spiegel ist dann **teilweise aktualisiert** (es gibt keinen „alten Stand“, auf den
  zurückgefallen werden könnte). `status` warnt; der nächste vollständige Lauf bringt ihn
  wieder auf Stand.
- Log abschließen, Lock entfernen, Exit-Code `3` (Abbruch) bzw. `1` (Fehler).

### 6.7 Warten auf ausstehende Schreibvorgänge

`sync(2)` in einem Hintergrund-Thread; währenddessen zeigt das Tool nach 2 s „… bitte warten,
nicht abziehen“ und jede Sekunde „noch X ausstehend“ (Summe aus `Dirty` und `Writeback` in
`/proc/meminfo`, gilt für alle Laufwerke). Strg+C beendet das Warten mit einer Warnung, der
Kernel schreibt aber weiter; der Lauf gilt dann als abgebrochen.

---

## 7. Dateiauswahl mit Git-Filter (`filter = "git"`)

Pro Mapping wählbar. Statt rsync-Excludes entscheidet **Git selbst**, welche Dateien
gesichert werden: verschachtelte `.gitignore`-Dateien, Negationen, `**`, `.git/info/exclude`
der vorhandenen Repos. Git dient **nur zur Auswahl**; kopiert wird mit rsync.

```toml
[[mapping]]
name    = "Programmieren"
source  = "~/Documents/Privat/Programmieren/"
target  = "Dokumente/Privat/Programmieren/"
filter  = "git"
exclude = ["*.iso"]          # gitignore-Syntax, gilt zusätzlich zu den .gitignore-Dateien
include = ["**/.env"]        # wird gesichert, obwohl .gitignore es ausschließt
backup_git_dirs = true       # .git-Ordner der gefundenen Repos mitsichern (Standard)
```

### Isolierte Git-Umgebung

- `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`; alle übrigen `GIT_*`-Variablen des
  Aufrufers werden entfernt; dazu `GIT_OPTIONAL_LOCKS=0`, `LC_ALL=C`.
- `-c core.excludesFile=<tmp>/excludes` mit `exclude` (+ ggf. `defaults.exclude`), gilt in
  **allen** Repos des Mappings. **Wird immer gesetzt, auch wenn die Datei leer ist:**
  `GIT_CONFIG_GLOBAL=/dev/null` allein reicht nicht, Git liest sonst trotzdem
  `~/.config/git/ignore` (mit git 2.56 geprüft).
- `-c core.fsmonitor=false`.
- `-z` bei allen Listen.

Temporäre Dateien liegen in `$XDG_RUNTIME_DIR` (Fallback `os.TempDir()`), nicht auf dem Stick.
Git verändert nichts in der Quelle.

### Dateiliste erstellen

**Repo-Wurzel**: Ordner mit einem Eintrag `.git` (Ordner oder Datei bei Worktrees/Submodulen).

1. **Mapping-Wurzel:** ist sie eine Repo-Wurzel → `git ls-files -co --exclude-standard -z`
   darin; sonst über ein leeres, temporäres **Hilfs-Repo** (`git init --bare`) mit
   `--git-dir=<hilfs-repo> --work-tree=<wurzel>`.
2. **Unter-Repos:** Git listet ein nicht verfolgtes Unter-Repo nur als `pfad/`, ein Submodul als
   `pfad`. Für jeden Eintrag, der ein Ordner und eine Repo-Wurzel ist, wird Schritt 1 rekursiv
   wiederholt – jedes Repo mit seinen eigenen Ignore-Regeln. Fehler in einem Unter-Repo (z.B.
   `safe.directory`) → Warnung, Unter-Repo übersprungen; in der Mapping-Wurzel → Fehler.
3. **Verfolgte, aber gelöschte Dateien** werden per `lstat` verworfen.
4. **`include`:** pro Repo zusätzlich `ls-files -o -i --exclude-from=<tmp>/includes -z` ohne
   `--exclude-standard` (findet auch `node_modules/.env`). Dauer pro Repo steht im Log.
5. **`.git`-Ordner:** bei `backup_git_dirs = true` alle Dateien darunter (bzw. die `.git`-Datei).
6. **Ergebnis:** sortierte, doppelfreie Liste einzelner Dateien und Symlinks, relativ zur
   Mapping-Wurzel. Symlinks überspringt rsync beim Kopieren.

Kopiert wird mit `--from0 --files-from=<tmp>/files` statt `--exclude`; **leere Ordner** werden
nicht gesichert. Die Liste wird pro Lauf einmal erstellt und für §6.2–§6.4 verwendet; `verify`
erstellt eine eigene.

### Hinweise für Benutzer

- Ignoriert ist nicht unwichtig: `.env`, lokale Konfigurationen, Datenbanken stehen oft in
  `.gitignore`. Dafür gibt es `include`.
- Ohne `backup_git_dirs` fehlen lokale, nicht gepushte Commits, Stashes und Branches.
- Auf exFAT gehen die Ausführungsrechte in `.git` verloren (z.B. Hooks); für ein
  Wiederherstellen des Repos selbst ist das unkritisch.

---

## 8. Lock

Datei `<medium>/.backup.lock`, angelegt mit `O_CREATE|O_EXCL`. Inhalt (JSON): Host, PID,
Benutzer, Startzeit, Befehl. Nur `run` setzt einen Lock.

| Situation | Verhalten |
|---|---|
| gleicher Host, PID läuft nicht mehr | veralteter Lock: anzeigen, nach Bestätigung entfernen |
| gleicher Host, PID läuft | Exit-Code `5` |
| anderer Host | Exit-Code `5` mit Hinweis auf `--force-unlock` |

`status` und `verify` zeigen einen aktiven Lock nur an.

---

## 9. Statusdatei und Log

### `.backup-state.json`

```json
{
  "format_version": 1,
  "tool_version": "0.2.0",
  "medium_id": "6f1c2e9a-…",
  "host": "schubsis-laptop",
  "user": "schubsi",
  "started_at": "2026-11-17T19:57:02Z",
  "finished_at": "2026-11-17T19:58:10Z",
  "status": "complete",
  "mappings": [
    {
      "name": "Minecraft",
      "source": "/home/schubsi/.minecraft/saves",
      "target": "Spiele/Minecraft/saves",
      "filter": "rsync",
      "host": "schubsis-laptop",
      "updated_at": "2026-11-17T19:58:09Z",
      "status": "ok",
      "files_total": 4812,
      "files_transferred": 37,
      "bytes_transferred": 18874368,
      "files_deleted": 2,
      "warnings": []
    }
  ],
  "warnings": []
}
```

`status`: `running` | `complete` | `aborted` | `failed`.
Mapping-`status`: `ok` | `warning` | `failed` | `skipped`.
`host`/`updated_at` pro Mapping zeigen, welcher Rechner das Mapping zuletzt geschrieben hat
(Grundlage für §6.2 a, auch bei `--only`). `files_total` zählt nur reguläre Dateien.
Geschrieben wird atomar (temporäre Datei + `rename`) mit `fsync`.

### Log

`logs/<YYYY-MM-DD_HHMMSS>_<host>.log` (bei Namensgleichheit `-2`, …), reiner Text, UTF-8:

- Kopf: Tool-Version, Host, Benutzer, Medium, Dateisystem, Mappings + Excludes
- Ergebnisse der Prüfungen aus §6.2 inkl. Antworten (bzw. „bestätigt durch --yes“)
- pro Mapping: rsync-Befehl, alle `--out-format`-Zeilen, stderr, Exit-Code
- Zusammenfassung und Ergebnis

Logs älter als 90 Tage löscht `run`.

---

## 10. Mehrere Rechner und Wiederherstellen

- Gleiche `backup.toml` für alle Rechner; `~` macht die Pfade unabhängig vom Benutzernamen.
- **Es gibt einen gemeinsamen Spiegel.** Haben zwei Rechner dasselbe Mapping mit
  unterschiedlichem Inhalt, gewinnt der zuletzt gesicherte; Dateien, die nur der andere
  Rechner hat, werden gelöscht. Davor schützt die Rückfrage aus §6.2 a. Wer auf mehreren
  Rechnern verschiedene Daten hat, gibt den Mappings verschiedene Targets.

Wiederherstellen (kein Befehl):

```sh
rsync -rt /run/media/schubsi/INTENSO/mirror/Spiele/Minecraft/saves/Survival/ \
          ~/.minecraft/saves/Survival/
meld /run/media/schubsi/INTENSO/mirror/Dokumente/Privat/Programmieren ~/Documents/Privat/Programmieren
```

Nach dem Zurückkopieren sind alle Dateien ausführbar (exFAT kennt keine Rechte); Symlinks
fehlen.

---

## 11. Implementierung

- Go ≥ 1.23, Modul `backup` (Pfad noch offen)
- Abhängigkeit: nur `github.com/BurntSushi/toml` (unbekannte Felder über `MetaData.Undecoded()`);
  CLI über `flag` mit eigenem Dispatch für Unterbefehle; UUID über `crypto/rand`
- statisches Binary (`CGO_ENABLED=0`)
- Meldungen auf Deutsch; Code, Bezeichner und Kommentare auf Englisch

```
cmd/stick-backup/    main, Unterbefehle, Flags
internal/app/        init, status, run, verify
internal/config/     Parsen, Validierung, Pfadauflösung, Vorlage
internal/medium/     mountinfo, lsblk (Wechseldatenträger), Dateisystem-Hinweise
internal/state/      .backup-state.json
internal/rsync/      Befehle bauen, ausführen, itemize/stats/list-only parsen
internal/gitfilter/  Dateiliste über Git
internal/lock/
internal/logx/       Log-Datei
internal/ui/         Ausgabe, Farben, Fragen, Fortschritt
```

### Tests

- **Unit-Tests:** Config (Targets, Überlappungen, unbekannte Felder, `~`), rsync (itemize,
  stats in drei Zahlenformaten, list-only, Argumente), mountinfo (`\040`), lsblk-Laufwerke,
  Statusdatei, Lock, Hostname, Kollisionserkennung, stderr-Pfade, Argument-Parsing.
- **Integrationstests** (echtes rsync/git in `t.TempDir()`, Medium als exFAT gemeldet):
  Spiegel, Excludes, Positivliste (inkl. neuer Ordner, entfernter Pfad, Tippfehler), Symlink-Hinweis, Änderung/Löschung, `--delete-excluded`, `--only`,
  Massen-Löschung mit „n“/„j“, `--dry-run` schreibt nichts, leere/fehlende Quelle, Abbruch,
  anderer Rechner neuer, Groß-/Kleinschreibung, Git-Filter (inkl. Entfernen nicht gelisteter
  Dateien und leerer Ordner), Locks, `verify` mit simuliertem Datenfehler, `init`-Auswahl.
  Git-Filter separat: Negation, verschachtelte `.gitignore`, Unter-Repos, Submodul/Worktree,
  gelöschte verfolgte Datei, `include` in ignoriertem Ordner, `backup_git_dirs`, globale
  Ignore-Datei des Benutzers wird nicht beachtet, Dateinamen mit Leerzeichen/Zeilenumbruch.
- **Manuell** mit einem exFAT-Image (`mkfs.exfat`, `udisksctl loop-setup`/`mount`, kein sudo).

---

## 12. Später (nicht Version 1)

- `udisksctl power-off` nach dem Auswerfen
- `--json` (ein JSON-Ereignis pro Zeile) für Integration in die Shell
- `restore`-Befehl

---

## 13. Offene Punkte

- **Standardwert** für `max_delete_percent` (20) bestätigen.
- **Symlinks:** nur Hinweis, keine Warnung – passt das, oder sollen sie als Textdatei
  (Ziel) gesichert werden?
- **Git-Filter, große ignorierte Ordner** (z.B. `node_modules/`) mit `include`: Laufzeit an
  echten Daten messen.
- **Mapping umbenannt oder Target verschoben:** Der alte Ordner bleibt in `mirror/` liegen und
  wird nicht mehr aktualisiert; `status` sollte ihn anzeigen, löschen muss man ihn von Hand.
