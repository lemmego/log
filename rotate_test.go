package log

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestARotatorCreatesItsDirectoryAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "app.log")

	rotator, err := NewRotator(RotateOptions{Path: path})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer rotator.Close()

	if _, err := rotator.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(contents) != "hello\n" {
		t.Fatalf("file holds %q", contents)
	}
}

// A log holds whatever was logged — a reset token, an address, a webhook
// body — so it is not world-readable.
func TestALogFileIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "app.log")

	rotator, err := NewRotator(RotateOptions{Path: path})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer rotator.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("file mode is %04o; it should not be readable by group or other", mode)
	}

	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dir.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("directory mode is %04o", mode)
	}
}

func TestRotationHappensBeforeTheLimitIsExceeded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	rotator, err := NewRotator(RotateOptions{Path: path, MaxSize: 64})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer rotator.Close()

	line := strings.Repeat("x", 20) + "\n"
	for range 10 {
		if _, err := rotator.Write([]byte(line)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// The active file must never have exceeded the limit.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > 64 {
		t.Fatalf("the active file is %d bytes, over the 64 limit", info.Size())
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) < 2 {
		t.Fatalf("nothing was rotated: %d files", len(entries))
	}
}

func TestPruningKeepsOnlyMaxFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	rotator, err := NewRotator(RotateOptions{Path: path, MaxSize: 16, MaxFiles: 2})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer rotator.Close()

	// Rotated names carry a timestamp to the second, so writes have to land
	// in distinguishable seconds to produce distinct files. Rather than
	// sleep, rotate by hand with a distinct name each time.
	for i := range 6 {
		if _, err := rotator.Write([]byte(strings.Repeat("y", 20) + "\n")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	entries, _ := os.ReadDir(dir)
	// The active file plus at most MaxFiles rotated ones.
	if len(entries) > 3 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("kept %d files, want at most 3: %v", len(entries), names)
	}
}

// A logger that panics turns a full disk into an outage, and one that drops
// silently is worse than no logger. A failed write goes to the fallback and
// says so once.
func TestAnUnwritableFileFallsBackAndSaysSoOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fallback := &strings.Builder{}
	rotator, err := NewRotator(RotateOptions{Path: path, Fallback: fallback})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}

	// Close the file underneath it, which is what a write to a closed or
	// removed descriptor looks like.
	rotator.file.Close()

	for range 5 {
		if _, err := rotator.Write([]byte("a record\n")); err != nil {
			t.Fatalf("Write returned an error instead of falling back: %v", err)
		}
	}

	written := fallback.String()
	if !strings.Contains(written, "a record") {
		t.Fatalf("the records did not reach the fallback: %q", written)
	}
	if count := strings.Count(written, "this is reported once"); count != 1 {
		t.Fatalf("the failure was reported %d times, want exactly 1", count)
	}
}

// NewRotator fails for a path it cannot open at all, so a misconfigured path
// is a boot failure rather than a log that goes nowhere.
func TestAnUnopenablePathIsAnError(t *testing.T) {
	if _, err := NewRotator(RotateOptions{Path: ""}); err == nil {
		t.Fatal("an empty path was accepted")
	}

	// A file where a directory has to go.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("setting up: %v", err)
	}
	if _, err := NewRotator(RotateOptions{Path: filepath.Join(blocker, "app.log")}); err == nil {
		t.Fatal("a path under a regular file was accepted")
	}
}

// slog calls a handler from whichever goroutine logged, so the writer has to
// serialise or two records interleave mid-line.
func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")

	rotator, err := NewRotator(RotateOptions{Path: path, MaxSize: -1})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer rotator.Close()

	const writers, each = 16, 40
	line := strings.Repeat("z", 64) + "\n"

	var wait sync.WaitGroup
	for range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range each {
				rotator.Write([]byte(line))
			}
		}()
	}
	wait.Wait()

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n")
	if len(lines) != writers*each {
		t.Fatalf("got %d lines, want %d", len(lines), writers*each)
	}
	for i, one := range lines {
		if len(one) != 64 {
			t.Fatalf("line %d is %d bytes, so two writes interleaved", i, len(one))
		}
	}
}
