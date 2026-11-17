// Package config parses and validates backup.toml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// FormatVersion is the newest config format this tool understands.
const FormatVersion = 1

// FileName is the name of the config file in the medium root.
const FileName = "backup.toml"

const (
	FilterRsync = "rsync"
	FilterGit   = "git"
)

type Config struct {
	FormatVersion int       `toml:"format_version"`
	Medium        Medium    `toml:"medium"`
	Safety        Safety    `toml:"safety"`
	Defaults      Defaults  `toml:"defaults"`
	Mappings      []Mapping `toml:"mapping"`
}

type Medium struct {
	ID   string `toml:"id"`
	Name string `toml:"name"`
}

type Safety struct {
	MaxDeletePercent int  `toml:"max_delete_percent"`
	AllowEmptySource bool `toml:"allow_empty_source"`
}

type Defaults struct {
	Exclude []string `toml:"exclude"`
}

type Mapping struct {
	Name               string   `toml:"name"`
	Source             string   `toml:"source"`
	Target             string   `toml:"target"`
	Exclude            []string `toml:"exclude"`
	UseDefaultExcludes *bool    `toml:"use_default_excludes"`
	Filter             string   `toml:"filter"`
	Include            []string `toml:"include"`
	BackupGitDirs      *bool    `toml:"backup_git_dirs"`
}

// UsesDefaultExcludes reports whether defaults.exclude applies (default true).
func (m *Mapping) UsesDefaultExcludes() bool {
	return m.UseDefaultExcludes == nil || *m.UseDefaultExcludes
}

// GitDirs reports whether .git directories are backed up (default true).
func (m *Mapping) GitDirs() bool { return m.BackupGitDirs == nil || *m.BackupGitDirs }

// IsGit reports whether the mapping uses the git filter.
func (m *Mapping) IsGit() bool { return m.Filter == FilterGit }

// CleanTarget returns the normalized target path.
func (m *Mapping) CleanTarget() string { return filepath.Clean(m.Target) }

// Excludes returns the effective exclude patterns of the mapping.
func (c *Config) Excludes(m *Mapping) []string {
	var out []string
	if m.UsesDefaultExcludes() {
		out = append(out, c.Defaults.Exclude...)
	}
	return append(out, m.Exclude...)
}

// HasGitMapping reports whether at least one mapping uses the git filter.
func (c *Config) HasGitMapping() bool {
	for i := range c.Mappings {
		if c.Mappings[i].IsGit() {
			return true
		}
	}
	return false
}

// MappingNames returns the names of all mappings in config order.
func (c *Config) MappingNames() []string {
	names := make([]string, len(c.Mappings))
	for i, m := range c.Mappings {
		names[i] = m.Name
	}
	return names
}

// TooNewError is returned when the config was written for a newer tool.
type TooNewError struct{ Version int }

func (e *TooNewError) Error() string {
	return fmt.Sprintf("format_version %d wird nicht unterstützt (höchstens %d): Tool zu alt, bitte aktualisieren", e.Version, FormatVersion)
}

// Load reads and parses a config file. It applies defaults and rejects unknown
// fields, but does not run the semantic validation (see Validate).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse parses config data, see Load.
func Parse(data []byte) (*Config, error) {
	cfg := &Config{Safety: Safety{MaxDeletePercent: 20}}
	md, err := toml.NewDecoder(bytes.NewReader(data)).Decode(cfg)
	if err != nil {
		return nil, fmt.Errorf("TOML-Fehler: %w", err)
	}
	if !md.IsDefined("format_version") {
		return nil, errors.New("format_version fehlt")
	}
	if cfg.FormatVersion > FormatVersion {
		return nil, &TooNewError{cfg.FormatVersion}
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, len(und))
		for i, k := range und {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unbekannte Felder: %s", strings.Join(keys, ", "))
	}
	// Migration of older format versions would happen here (in memory only).
	for i := range cfg.Mappings {
		if cfg.Mappings[i].Filter == "" {
			cfg.Mappings[i].Filter = FilterRsync
		}
	}
	return cfg, nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidUUID reports whether s is a UUID in canonical text form.
func ValidUUID(s string) bool { return uuidRe.MatchString(s) }

// Result of the semantic validation.
type Result struct {
	Errors   []string
	Warnings []string
}

func (r *Result) errf(format string, a ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, a...))
}
func (r *Result) warnf(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// OK reports whether there are no errors.
func (r *Result) OK() bool { return len(r.Errors) == 0 }

// Err returns all errors joined into one error, or nil.
func (r *Result) Err() error {
	if r.OK() {
		return nil
	}
	return errors.New("ungültige Konfiguration:\n  - " + strings.Join(r.Errors, "\n  - "))
}

// Validate checks the config. home is used to resolve "~" in sources for the
// overlap check.
func (c *Config) Validate(home string) *Result {
	r := &Result{}
	if c.FormatVersion < 1 {
		r.errf("format_version %d ist ungültig", c.FormatVersion)
	}
	if !ValidUUID(c.Medium.ID) {
		r.errf("medium.id %q ist keine gültige UUID", c.Medium.ID)
	}
	if strings.TrimSpace(c.Medium.Name) == "" {
		r.errf("medium.name fehlt")
	}
	if c.Safety.MaxDeletePercent < 0 || c.Safety.MaxDeletePercent > 100 {
		r.errf("safety.max_delete_percent muss zwischen 0 und 100 liegen")
	}
	for _, p := range c.Defaults.Exclude {
		if strings.TrimSpace(p) == "" {
			r.errf("defaults.exclude enthält ein leeres Muster")
		}
	}
	defaultsHaveNegation := false
	for _, p := range c.Defaults.Exclude {
		if strings.HasPrefix(p, "!") {
			defaultsHaveNegation = true
		}
	}

	seenNames := map[string]bool{}
	type tgt struct{ name, path string }
	var targets []tgt
	type src struct{ name, path string }
	var sources []src

	for i := range c.Mappings {
		m := &c.Mappings[i]
		label := m.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		if strings.TrimSpace(m.Name) == "" {
			r.errf("Mapping %s: name fehlt", label)
		} else {
			key := strings.ToLower(m.Name)
			if seenNames[key] {
				r.errf("Mapping-Name %q ist doppelt (Groß-/Kleinschreibung wird ignoriert)", m.Name)
			}
			seenNames[key] = true
		}
		if m.Filter != FilterRsync && m.Filter != FilterGit {
			r.errf("Mapping %s: filter %q ist ungültig (erlaubt: \"rsync\", \"git\")", label, m.Filter)
		}
		if m.Source == "" {
			r.errf("Mapping %s: source fehlt", label)
		} else if p, err := ExpandSource(m.Source, home); err != nil {
			r.errf("Mapping %s: %v", label, err)
		} else {
			sources = append(sources, src{label, p})
		}
		if err := ValidateTarget(m.Target); err != nil {
			r.errf("Mapping %s: %v", label, err)
		} else {
			targets = append(targets, tgt{label, m.CleanTarget()})
		}
		for _, p := range m.Exclude {
			if strings.TrimSpace(p) == "" {
				r.errf("Mapping %s: exclude enthält ein leeres Muster", label)
			}
		}
		if m.Filter == FilterRsync {
			for _, p := range m.Exclude {
				if strings.HasPrefix(p, "!") {
					r.errf("Mapping %s: Negation %q wird bei filter = \"rsync\" nicht unterstützt", label, p)
				}
			}
			if m.UsesDefaultExcludes() && defaultsHaveNegation {
				r.errf("Mapping %s: defaults.exclude enthält Negationen (!), das ist nur bei filter = \"git\" erlaubt (oder use_default_excludes = false)", label)
			}
			for _, p := range m.Include {
				if strings.HasPrefix(p, "!") {
					r.errf("Mapping %s: include %q: Negation wird bei filter = \"rsync\" nicht unterstützt", label, p)
				}
				for _, seg := range strings.Split(p, "/") {
					if seg == ".." {
						r.errf("Mapping %s: include %q darf kein \"..\" enthalten", label, p)
						break
					}
				}
			}
			if m.BackupGitDirs != nil {
				r.errf("Mapping %s: backup_git_dirs ist nur bei filter = \"git\" erlaubt", label)
			}
		}
		for _, p := range m.Include {
			if strings.TrimSpace(p) == "" {
				r.errf("Mapping %s: include enthält ein leeres Muster", label)
			}
		}
	}

	sort.Slice(targets, func(i, j int) bool { return targets[i].path < targets[j].path })
	for i := 0; i < len(targets); i++ {
		for j := i + 1; j < len(targets); j++ {
			a, b := targets[i], targets[j]
			if a.path == b.path {
				r.errf("Mappings %s und %s haben dasselbe Ziel %q", a.name, b.name, a.path)
			} else if isPathPrefix(a.path, b.path) || isPathPrefix(b.path, a.path) {
				r.errf("Ziele überlappen: %q (%s) und %q (%s)", a.path, a.name, b.path, b.name)
			}
		}
	}
	for i := 0; i < len(sources); i++ {
		for j := i + 1; j < len(sources); j++ {
			a, b := sources[i], sources[j]
			if a.path == b.path || isPathPrefix(a.path, b.path) || isPathPrefix(b.path, a.path) {
				r.warnf("Quellen überlappen: %q (%s) und %q (%s), Dateien werden doppelt gesichert", a.path, a.name, b.path, b.name)
			}
		}
	}
	if len(c.Mappings) == 0 {
		r.warnf("keine Mappings konfiguriert")
	}
	return r
}

// ValidateTarget checks a mapping target.
func ValidateTarget(t string) error {
	if t == "" {
		return errors.New("target fehlt")
	}
	if filepath.IsAbs(t) {
		return fmt.Errorf("target %q muss relativ sein", t)
	}
	c := filepath.Clean(t)
	if c == "." || c == "" {
		return fmt.Errorf("target %q ist leer", t)
	}
	for _, seg := range strings.Split(c, "/") {
		if seg == ".." {
			return fmt.Errorf("target %q darf kein \"..\" enthalten", t)
		}
	}
	return nil
}

// ExpandSource resolves "~" and checks that the result is absolute. It does
// not touch the file system.
func ExpandSource(s, home string) (string, error) {
	p := s
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home == "" {
			return "", errors.New("Home-Verzeichnis unbekannt, ~ kann nicht aufgelöst werden")
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	} else if strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("source %q: ~benutzer wird nicht unterstützt, bitte ~/ verwenden", s)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("source %q muss absolut sein oder mit ~/ beginnen", s)
	}
	return filepath.Clean(p), nil
}

// isPathPrefix reports whether dir is a proper path-wise prefix of p.
func isPathPrefix(dir, p string) bool {
	if dir == "/" {
		return p != "/"
	}
	return strings.HasPrefix(p, dir+"/")
}
