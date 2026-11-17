package medium

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `22 1 0:21 / /proc rw,nosuid shared:5 - proc proc rw
48 1 0:39 /root / rw,relatime shared:1 - btrfs /dev/mapper/luks-a0 rw,subvol=/root
126 48 259:8 / /boot rw,relatime shared:170 - ext4 /dev/nvme0n1p8 rw
174 126 259:1 / /boot/efi rw,relatime shared:175 - vfat /dev/nvme0n1p1 rw
900 48 253:3 / /run/media/schubsi/Backup\040Stick rw,nosuid shared:500 - ext4 /dev/mapper/luks-3f2a rw
901 48 0:50 / /tmp rw shared:9 - tmpfs tmpfs rw
902 48 8:17 / /run/media/schubsi/STICK rw - vfat /dev/sdb1 rw
903 48 0:60 / /mnt/a\134b\011c rw master:1 propagate_from:2 - ext4 /dev/sdc1 rw
`

func TestParseMountinfo(t *testing.T) {
	ms, err := ParseMountinfo(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 8 {
		t.Fatalf("got %d mounts", len(ms))
	}
	if ms[4].MountPoint != "/run/media/schubsi/Backup Stick" || ms[4].FSType != "ext4" || ms[4].Source != "/dev/mapper/luks-3f2a" {
		t.Errorf("escaped space: %+v", ms[4])
	}
	if ms[7].MountPoint != "/mnt/a\\b\tc" {
		t.Errorf("escapes: %q", ms[7].MountPoint)
	}
	if ms[1].Root != "/root" {
		t.Errorf("root: %q", ms[1].Root)
	}
	if _, err := ParseMountinfo(strings.NewReader("garbage\n")); err == nil {
		t.Error("expected error")
	}
}

func TestUnescapeEdge(t *testing.T) {
	for in, want := range map[string]string{`a\04`: `a\04`, `\040`: " ", `x\`: `x\`, `\999`: `\999`} {
		if got := unescape(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestMountFor(t *testing.T) {
	ms, _ := ParseMountinfo(strings.NewReader(sample))
	cases := map[string]string{
		"/run/media/schubsi/Backup Stick/x": "/run/media/schubsi/Backup Stick",
		"/boot/efi/EFI":                     "/boot/efi",
		"/bootx":                            "/",
		"/home/u":                           "/",
	}
	for p, want := range cases {
		m, ok := MountFor(ms, p)
		if !ok || m.MountPoint != want {
			t.Errorf("%s: got %q", p, m.MountPoint)
		}
	}
}

func TestFindCandidates(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	bind := filepath.Join(dir, "bind")
	bad := filepath.Join(dir, "bad")
	none := filepath.Join(dir, "none")
	for _, d := range []string{good, bind, bad, none} {
		os.Mkdir(d, 0o755)
	}
	cfg := "format_version = 1\n[medium]\nid = \"6f1c2e9a-8b4d-4f3a-9c51-2d7e0b8a4f11\"\nname = \"x\"\n"
	os.WriteFile(filepath.Join(good, "backup.toml"), []byte(cfg), 0o644)
	os.WriteFile(filepath.Join(bind, "backup.toml"), []byte(cfg), 0o644)
	os.WriteFile(filepath.Join(bad, "backup.toml"), []byte("nonsense = = ="), 0o644)
	ms := []Mount{
		{MountPoint: good, FSType: "ext4"},
		{MountPoint: good, FSType: "ext4"},
		{MountPoint: bind, FSType: "ext4"},
		{MountPoint: bad, FSType: "ext4"},
		{MountPoint: none, FSType: "ext4"},
		{MountPoint: dir, FSType: "tmpfs"},
	}
	c, w := FindCandidates(ms)
	if len(c) != 1 || c[0].Path != good {
		t.Errorf("candidates: %+v", c)
	}
	if len(w) != 1 || !strings.Contains(w[0], bad) {
		t.Errorf("warnings: %v", w)
	}
}

func TestCheckFS(t *testing.T) {
	if CheckFS("vfat") == "" {
		t.Error("vfat should warn about 4 GB")
	}
	for _, fs := range []string{"exfat", "ext4", "ntfs3"} {
		if w := CheckFS(fs); w != "" {
			t.Errorf("%s: %q", fs, w)
		}
	}
}

func TestParseDrives(t *testing.T) {
	data := `{"blockdevices":[
	 {"name":"sda","path":"/dev/sda","fstype":null,"mountpoints":[null],"size":64000000000,"label":null,"hotplug":true,"children":[
	   {"name":"sda1","path":"/dev/sda1","fstype":"crypto_LUKS","mountpoints":[null],"size":64000000000,"label":null,"hotplug":true,"children":[
	     {"name":"luks-x","path":"/dev/mapper/luks-x","fstype":"ext4","mountpoints":["/run/media/u/Backup"],"size":63000000000,"label":"Backup","hotplug":false}]}]},
	 {"name":"sdb1","path":"/dev/sdb1","fstype":"vfat","mountpoints":["/run/media/u/STICK"],"size":"8000000000","label":"STICK","hotplug":"1"},
	 {"name":"nvme0n1p3","path":"/dev/nvme0n1p3","fstype":"btrfs","mountpoints":["/","/home"],"size":1000,"label":null,"hotplug":false},
	 {"name":"sdc1","path":"/dev/sdc1","fstype":"ext4","mountpoints":[null],"size":1000,"label":null,"hotplug":true}
	]}`
	d, err := ParseDrives([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 3 || d[2].Device != "/dev/sdc1" || d[2].Mounted() {
		t.Fatalf("drives: %+v", d)
	}
	if d[0] != (Drive{Device: "/dev/mapper/luks-x", MountPoint: "/run/media/u/Backup", FSType: "ext4", Label: "Backup", Size: 63000000000}) {
		t.Errorf("luks drive: %+v", d[0])
	}
	if d[1].MountPoint != "/run/media/u/STICK" || d[1].FSType != "vfat" || d[1].Size != 8000000000 {
		t.Errorf("vfat drive: %+v", d[1])
	}
}

func TestParseMountOutput(t *testing.T) {
	for in, want := range map[string]string{
		"Mounted /dev/sda1 at /run/media/u/INTENSO\n":   "/run/media/u/INTENSO",
		"Mounted /dev/sda1 at /run/media/u/IN TENSO.\n": "/run/media/u/IN TENSO",
	} {
		if got, ok := ParseMountOutput(in); !ok || got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
	if _, ok := ParseMountOutput("Error mounting"); ok {
		t.Error("error parsed")
	}
}

func TestPendingWrites(t *testing.T) {
	if PendingWrites() < 0 {
		t.Error("meminfo not readable")
	}
}
