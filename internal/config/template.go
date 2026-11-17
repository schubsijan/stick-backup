package config

import (
	"crypto/rand"
	"fmt"
	"strconv"
)

// NewUUID returns a random version 4 UUID.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Template returns the content of a fresh backup.toml.
func Template(id, name string) string {
	return `format_version = 1

# Das Tool hält auf dem Medium einen Spiegel des letzten Stands (Ordner mirror/).
# Es gibt keine älteren Versionen: Was in der Quelle gelöscht wird, verschwindet
# beim nächsten Lauf auch hier.

[medium]
id   = "` + id + `"   # von ` + "`stick-backup init`" + ` erzeugt, nicht ändern
name = ` + strconv.Quote(name) + `   # frei wählbar, nur zur Anzeige

[safety]
max_delete_percent = 20   # mehr Löschungen gegenüber dem Spiegel → Bestätigung nötig
allow_empty_source = false

[defaults]
exclude = ["*.tmp", "*.swp", "**/.cache/"]

# Beispiele – zum Aktivieren die Raute (#) am Zeilenanfang entfernen.
#
# [[mapping]]
# name    = "Programmieren"
# source  = "~/Documents/Privat/Programmieren/"
# target  = "Dokumente/Privat/Programmieren/"
# exclude = ["**/node_modules/", "**/target/", "**/__pycache__/", "*.o"]
#
# [[mapping]]
# name   = "Minecraft"
# source = "~/.minecraft/saves/"
# target = "Spiele/Minecraft/saves/"
# use_default_excludes = false   # Welten vollständig sichern
#
# Dateiauswahl über Git (.gitignore-Dateien aller Repos werden beachtet):
#
# [[mapping]]
# name    = "Quelltexte"
# source  = "~/src/"
# target  = "Quelltexte/"
# filter  = "git"
# exclude = ["*.iso"]          # gitignore-Syntax
# include = ["**/.env"]        # trotz .gitignore sichern
# backup_git_dirs = true       # .git-Ordner mitsichern
`
}
