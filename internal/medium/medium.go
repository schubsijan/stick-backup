// Package medium finds backup media among the mounted file systems.
package medium

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/schubsijan/stick-backup/internal/config"
)

// Mount is one line of /proc/self/mountinfo.
type Mount struct {
	MountPoint string
	Root       string
	FSType     string
	Source     string
}

// ParseMountinfo parses the format of /proc/<pid>/mountinfo.
func ParseMountinfo(r io.Reader) ([]Mount, error) {
	var out []Mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			return nil, fmt.Errorf("mountinfo: Zeile ohne Trenner: %q", line)
		}
		f := strings.Fields(pre)
		g := strings.Fields(post)
		if len(f) < 5 || len(g) < 2 {
			return nil, fmt.Errorf("mountinfo: unvollständige Zeile: %q", line)
		}
		out = append(out, Mount{
			Root:       unescape(f[3]),
			MountPoint: unescape(f[4]),
			FSType:     g[0],
			Source:     unescape(g[1]),
		})
	}
	return out, sc.Err()
}

// unescape decodes the octal escapes (\040 etc.) used in mountinfo.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOct(s[i+1]) && isOct(s[i+2]) && isOct(s[i+3]) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOct(c byte) bool { return c >= '0' && c <= '7' }

// ReadMounts reads the mount table of the current process.
func ReadMounts() ([]Mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseMountinfo(f)
}

var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "tmpfs": true, "devtmpfs": true, "overlay": true,
	"squashfs": true, "fuse.portal": true, "autofs": true, "mqueue": true, "debugfs": true,
	"tracefs": true, "securityfs": true, "pstore": true, "bpf": true, "configfs": true,
	"fusectl": true, "hugetlbfs": true, "devpts": true, "binfmt_misc": true, "efivarfs": true,
	"selinuxfs": true, "rpc_pipefs": true, "nsfs": true, "ramfs": true, "nfsd": true,
	"fuse.gvfsd-fuse": true, "swap": true,
}

func skipMount(m Mount) bool {
	if pseudoFS[m.FSType] || strings.HasPrefix(m.FSType, "cgroup") {
		return true
	}
	switch m.MountPoint {
	case "/", "/boot", "/boot/efi":
		return true
	}
	for _, p := range []string{"/proc", "/sys", "/dev"} {
		if m.MountPoint == p || strings.HasPrefix(m.MountPoint, p+"/") {
			return true
		}
	}
	return false
}

// Candidate is a mounted backup medium.
type Candidate struct {
	Path   string
	FSType string
	Device string
	Config *config.Config
}

// FindCandidates returns all mount points with a usable backup.toml, plus
// warnings about invalid config files.
func FindCandidates(mounts []Mount) ([]Candidate, []string) {
	var out []Candidate
	var warns []string
	seenPath := map[string]bool{}
	seenID := map[string]bool{}
	for _, m := range mounts {
		if skipMount(m) || seenPath[m.MountPoint] {
			continue
		}
		seenPath[m.MountPoint] = true
		p := filepath.Join(m.MountPoint, config.FileName)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		cfg, err := config.Load(p)
		if err != nil {
			warns = append(warns, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		if cfg.Medium.ID == "" {
			warns = append(warns, fmt.Sprintf("%s: medium.id fehlt", p))
			continue
		}
		if seenID[cfg.Medium.ID] {
			continue
		}
		seenID[cfg.Medium.ID] = true
		out = append(out, Candidate{Path: m.MountPoint, FSType: m.FSType, Device: m.Source, Config: cfg})
	}
	return out, warns
}

// MountFor returns the mount that contains path (longest mount point prefix,
// last mount wins for stacked mounts). path should be absolute and resolved.
func MountFor(mounts []Mount, path string) (Mount, bool) {
	var best Mount
	found := false
	for _, m := range mounts {
		mp := m.MountPoint
		if path == mp || mp == "/" || strings.HasPrefix(path, mp+"/") {
			if !found || len(mp) >= len(best.MountPoint) {
				best, found = m, true
			}
		}
	}
	return best, found
}

// CheckFS returns a warning (empty if none) about limits of the file system.
// The tool only uses features every common file system has (files,
// directories, mtimes), so no file system is refused.
func CheckFS(fstype string) string {
	switch fstype {
	case "vfat", "msdos":
		return "FAT32: Dateien über 4 GB können nicht gesichert werden (exFAT hat diese Grenze nicht)"
	}
	return ""
}

// Space returns total and available bytes of the file system containing path.
func Space(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), nil
}

// BlockDevice is one entry of lsblk --json output.
type BlockDevice struct {
	Name        string        `json:"name"`
	Path        string        `json:"path"`
	FSType      *string       `json:"fstype"`
	MountPoints []*string     `json:"mountpoints"`
	Size        any           `json:"size"`
	Label       *string       `json:"label"`
	Hotplug     any           `json:"hotplug"` // bool, or "0"/"1" in older lsblk
	Children    []BlockDevice `json:"children"`
}

func (d *BlockDevice) hotplug() bool {
	switch v := d.Hotplug.(type) {
	case bool:
		return v
	case string:
		return v == "1" || v == "true"
	case float64:
		return v == 1
	}
	return false
}

func (d *BlockDevice) fstype() string {
	if d.FSType == nil {
		return ""
	}
	return *d.FSType
}
