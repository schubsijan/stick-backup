package main

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		args            string
		cmd, target     string
		pos             string
		yes, dry, pause bool
		only            string
	}{
		{"--target /m run --dry-run", "run", "/m", "", false, true, false, ""},
		{"run --target /m -y --only a,b --checksum", "run", "/m", "", true, false, false, "a|b"},
		{"-y verify --pause", "verify", "", "", true, false, true, ""},
		{"--yes init /x", "init", "", "/x", true, false, false, ""},
	}
	for _, c := range cases {
		f, cmd, pos, err := parse(strings.Fields(c.args))
		if err != nil {
			t.Errorf("%q: %v", c.args, err)
			continue
		}
		if cmd != c.cmd || f.Target != c.target || strings.Join(pos, " ") != c.pos || f.Yes != c.yes || f.DryRun != c.dry || f.pause != c.pause || strings.Join(f.Only, "|") != c.only {
			t.Errorf("%q: cmd=%q target=%q pos=%v yes=%v dry=%v pause=%v only=%v", c.args, cmd, f.Target, pos, f.Yes, f.DryRun, f.pause, f.Only)
		}
	}
	if f, _, _, _ := parse([]string{"run", "--checksum"}); !f.Checksum {
		t.Error("--checksum not parsed")
	}
	if _, _, _, err := parse([]string{"status", "--dry-run"}); err == nil {
		t.Error("--dry-run must be rejected for status")
	}
}
