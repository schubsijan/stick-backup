package medium

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// SyncAll flushes all pending writes of all file systems (sync(2)), so data
// for the stick really is on the stick. On slow USB sticks this can take
// minutes; it cannot be interrupted.
func SyncAll() { syscall.Sync() }

// PendingWrites returns the bytes the kernel still has to write to disk
// (Dirty + Writeback from /proc/meminfo). The value covers all devices; it
// is used to show progress while waiting for a slow stick.
func PendingWrites() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return -1
	}
	defer f.Close()
	var sum int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || (k != "Dirty" && k != "Writeback") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		if err == nil {
			sum += n * 1024
		}
	}
	return sum
}
