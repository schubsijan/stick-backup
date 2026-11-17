package state

import (
	"testing"
	"time"
)

func TestReadWrite(t *testing.T) {
	dir := t.TempDir()
	if s, err := Read(dir); s != nil || err != nil {
		t.Fatalf("empty: %v %v", s, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	in := &State{FormatVersion: 1, Host: "h", StartedAt: now, Status: StatusComplete,
		Mappings: []Mapping{{Name: "A", Target: "a/b", Host: "h", Status: MappingWarning, Warnings: []string{"w"}}}}
	if err := Write(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(dir)
	if err != nil || out.Host != "h" || !out.StartedAt.Equal(now) || out.Mapping("a/b") == nil || !out.Mapping("a/b").Succeeded() || out.WarningCount() != 1 {
		t.Fatalf("roundtrip: %v %+v", err, out)
	}
	if out.Mapping("x") != nil {
		t.Error("unknown target found")
	}
	if CleanHost("my_host.local") != "my-host-local" {
		t.Error("CleanHost")
	}
}
