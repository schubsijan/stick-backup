package lock

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestLock(t *testing.T) {
	dir := t.TempDir()
	me := Info{Host: "h", PID: os.Getpid(), User: "u", StartedAt: time.Now(), Command: "run"}
	l, err := Acquire(dir, me)
	if err != nil {
		t.Fatal(err)
	}
	var he *HeldError
	if _, err := Acquire(dir, me); !errors.As(err, &he) || he.State != Active {
		t.Fatalf("second acquire: %v", err)
	}
	if _, err := Acquire(dir, Info{Host: "other", PID: 1}); !errors.As(err, &he) || he.State != Foreign {
		t.Fatalf("foreign: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := Inspect(dir, "h"); err != nil {
		t.Fatalf("after release: %v", err)
	}

	// Stale lock: a PID that does not exist.
	dead := Info{Host: "h", PID: 1 << 22, Command: "run"}
	if _, err := Acquire(dir, dead); err != nil {
		t.Fatal(err)
	}
	if err := Inspect(dir, "h"); !errors.As(err, &he) || he.State != Stale {
		t.Fatalf("stale: %v", err)
	}
}
