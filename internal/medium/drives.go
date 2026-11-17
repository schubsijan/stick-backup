package medium

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Drive is a file system on a hot-pluggable device (USB etc.).
type Drive struct {
	Device     string
	MountPoint string // "" if not mounted
	FSType     string
	Label      string
	Size       int64
}

func (d *BlockDevice) size() int64 {
	switch v := d.Size.(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// Mounted reports whether the drive is mounted.
func (d Drive) Mounted() bool { return d.MountPoint != "" }

// ParseDrives returns all file systems on hot-pluggable devices from
// `lsblk --json --bytes` output, mounted or not. Hotplug is inherited from
// parent devices, so an unlocked LUKS container on a USB stick counts as
// well. Locked LUKS containers and swap are skipped.
func ParseDrives(data []byte) ([]Drive, error) {
	var doc struct {
		BlockDevices []BlockDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var out []Drive
	seen := map[string]bool{}
	var walk func(d *BlockDevice, parentHotplug bool)
	walk = func(d *BlockDevice, parentHotplug bool) {
		hot := parentHotplug || d.hotplug()
		if hot && d.fstype() != "" && d.fstype() != "crypto_LUKS" && d.fstype() != "swap" {
			dr := Drive{Device: d.Path, FSType: d.fstype(), Size: d.size()}
			if dr.Device == "" {
				dr.Device = "/dev/" + d.Name
			}
			if d.Label != nil {
				dr.Label = *d.Label
			}
			for _, m := range d.MountPoints {
				if m != nil && *m != "" {
					dr.MountPoint = *m // first mount point is enough
					break
				}
			}
			if !seen[dr.Device] {
				seen[dr.Device] = true
				out = append(out, dr)
			}
		}
		for i := range d.Children {
			walk(&d.Children[i], hot)
		}
	}
	for i := range doc.BlockDevices {
		walk(&doc.BlockDevices[i], false)
	}
	return out, nil
}

func lsblk(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "lsblk", "--json", "--bytes", "-o", "NAME,PATH,FSTYPE,MOUNTPOINTS,SIZE,LABEL,HOTPLUG")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}

// RemovableDrives runs lsblk and returns removable drives (mounted or not);
// errors yield an empty list.
func RemovableDrives(ctx context.Context) []Drive {
	out, err := lsblk(ctx)
	if err != nil {
		return nil
	}
	d, _ := ParseDrives(out)
	return d
}

var mountedRe = regexp.MustCompile(`(?m)^Mounted \S+ at (.+?)\.?$`)

// MountDrive mounts a device via udisks (no root needed for removable drives
// of the active session) and returns the mount point.
func MountDrive(ctx context.Context, device string) (string, error) {
	cmd := exec.CommandContext(ctx, "udisksctl", "mount", "-b", device, "--no-user-interaction")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "NotAuthorized") {
			msg += " (keine Berechtigung – im Dateimanager einhängen)"
		}
		return "", fmt.Errorf("%s einhängen: %v: %s", device, err, msg)
	}
	mp, ok := ParseMountOutput(string(out))
	if !ok {
		return "", fmt.Errorf("%s einhängen: unerwartete Ausgabe: %s", device, strings.TrimSpace(string(out)))
	}
	return mp, nil
}

// ParseMountOutput extracts the mount point from `udisksctl mount` output.
func ParseMountOutput(out string) (string, bool) {
	m := mountedRe.FindStringSubmatch(out)
	if m == nil {
		return "", false
	}
	return strings.Trim(m[1], "`'\""), true
}

// UnmountDrive unmounts a device via udisks (flushes and marks the file
// system clean, so the stick can be pulled).
func UnmountDrive(ctx context.Context, device string) error {
	cmd := exec.CommandContext(ctx, "udisksctl", "unmount", "-b", device, "--no-user-interaction")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "busy") {
			msg += " (ein Programm oder Terminal benutzt den Stick noch, z.B. ein offenes Dateimanager-Fenster)"
		}
		return fmt.Errorf("%s aushängen: %v: %s", device, err, msg)
	}
	return nil
}
