package log

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// File permissions for a log file and its directory.
//
// 0600 rather than 0644, and that is not fussiness. A log holds whatever the
// application logged: a verification link, a password-reset token, a
// customer's email address, the body of a webhook. Laravel ships 0664 and
// every guide tells you to loosen it further so the web server can read it,
// which is how a log ends up world-readable on a shared host. Anything that
// needs to read these can be given the group or run as the owner.
const (
	FileMode = os.FileMode(0o600)
	DirMode  = os.FileMode(0o700)
)

// DefaultMaxSize is when a file rotates, in bytes.
const DefaultMaxSize int64 = 32 << 20 // 32 MiB

// DefaultMaxFiles is how many rotated files are kept.
//
// Seven, so a week of daily rotation survives, which is the window in which
// somebody actually asks "what happened on Tuesday".
const DefaultMaxFiles = 7

// RotateOptions configures a Rotator.
type RotateOptions struct {
	// Path is the active file. Its directory is created if it does not
	// exist.
	Path string

	// MaxSize is the size at which the file rotates. Zero means
	// DefaultMaxSize. Negative disables size-based rotation.
	MaxSize int64

	// Daily also rotates at the first write after midnight, local time.
	//
	// Worth having alongside the size limit rather than instead of it: a
	// size limit stops the disk filling and a date in the filename is what
	// makes a file findable. Laravel's daily channel does only the second,
	// so a bad day can still write forty gigabytes.
	Daily bool

	// MaxFiles is how many rotated files to keep, newest first. Zero means
	// DefaultMaxFiles. Negative keeps everything, which is a decision to
	// make deliberately.
	MaxFiles int

	// Fallback is where writes go when the file cannot be written. Defaults
	// to os.Stderr.
	//
	// There is always one. A logger that returns an error nobody checks has
	// dropped the message, and a logger that panics has turned a full disk
	// into an outage — so a failed write lands somewhere a process manager
	// captures instead.
	Fallback io.Writer
}

// Rotator is an io.Writer that writes to a file and rotates it.
//
// Safe for concurrent use: slog calls a handler from whichever goroutine
// logged, so the writer underneath has to serialise its own writes or two
// records interleave mid-line.
//
// Opened with O_APPEND, which on POSIX makes a single write of less than
// PIPE_BUF atomic with respect to other appenders — so two processes sharing
// one file produce interleaved records rather than interleaved bytes. That is
// the best available without a lock file, and it is why a record is written
// in one call rather than assembled with several.
type Rotator struct {
	opts RotateOptions

	mu     sync.Mutex
	file   *os.File
	size   int64
	day    string // the file's date, for Daily
	failed bool   // whether the fallback warning has been emitted
}

// NewRotator opens the file, creating its directory.
//
// It returns an error for a path it cannot open at all, so a misconfigured
// path is a boot failure rather than a log that silently goes nowhere. Once
// open, a later failure falls back instead of erroring — by then the process
// is serving requests and losing the log is better than stopping.
func NewRotator(opts RotateOptions) (*Rotator, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, fmt.Errorf("log: a rotator needs a path")
	}
	if opts.MaxSize == 0 {
		opts.MaxSize = DefaultMaxSize
	}
	if opts.MaxFiles == 0 {
		opts.MaxFiles = DefaultMaxFiles
	}
	if opts.Fallback == nil {
		opts.Fallback = os.Stderr
	}

	r := &Rotator{opts: opts}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// Path is the active file, so a startup line can say where the log is.
//
// Worth exposing: "where is the log file" is the first question anybody has,
// and answering it from configuration they have to find is worse than the
// application saying so.
func (r *Rotator) Path() string { return r.opts.Path }

func (r *Rotator) open() error {
	if err := os.MkdirAll(filepath.Dir(r.opts.Path), DirMode); err != nil {
		return fmt.Errorf("log: creating %s: %w", filepath.Dir(r.opts.Path), err)
	}

	file, err := os.OpenFile(r.opts.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, FileMode)
	if err != nil {
		return fmt.Errorf("log: opening %s: %w", r.opts.Path, err)
	}

	size := int64(0)
	day := time.Now().Format("2006-01-02")
	if info, err := file.Stat(); err == nil {
		size = info.Size()
		// The existing file's own date, not today's: a process starting on
		// Tuesday against a file last written on Monday must rotate it
		// rather than append a day's worth of records under the wrong date.
		day = info.ModTime().Format("2006-01-02")
	}

	r.file, r.size, r.day = file, size, day
	return nil
}

func (r *Rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return r.fallback(p, fmt.Errorf("log file is not open"))
	}

	if r.shouldRotate(int64(len(p))) {
		if err := r.rotate(); err != nil {
			// Rotation failed — a read-only directory, a permission change
			// under us. Keep writing to the file we have: an oversized log
			// is a smaller problem than no log.
			r.warn(fmt.Errorf("rotating %s: %w", r.opts.Path, err))
		}
	}

	written, err := r.file.Write(p)
	if err != nil {
		return r.fallback(p, err)
	}
	r.size += int64(written)
	return written, nil
}

// shouldRotate decides before a write, so the limit is a ceiling rather than
// something the file is already over by the time it is noticed.
func (r *Rotator) shouldRotate(incoming int64) bool {
	if r.opts.MaxSize > 0 && r.size+incoming > r.opts.MaxSize {
		return true
	}
	if r.opts.Daily && time.Now().Format("2006-01-02") != r.day {
		return true
	}
	return false
}

// rotate closes the active file, renames it with a timestamp, and reopens.
//
// The rotated name carries the date and time rather than a bare .1/.2
// sequence: renaming N files on every rotation is N syscalls that can fail
// halfway and leave a gap, and ".1" tells you nothing about when it was.
func (r *Rotator) rotate() error {
	if err := r.file.Close(); err != nil {
		r.warn(fmt.Errorf("closing %s: %w", r.opts.Path, err))
	}
	r.file = nil

	extension := filepath.Ext(r.opts.Path)
	base := strings.TrimSuffix(r.opts.Path, extension)
	rotated := fmt.Sprintf("%s-%s%s", base, time.Now().Format("2006-01-02-150405"), extension)

	if err := os.Rename(r.opts.Path, rotated); err != nil && !os.IsNotExist(err) {
		// Reopen the original before returning, or every later write
		// fails.
		_ = r.open()
		return err
	}

	if err := r.open(); err != nil {
		return err
	}
	r.prune()
	return nil
}

// prune removes the oldest rotated files beyond MaxFiles.
//
// Best effort and never fatal: a log that cannot tidy up is still a log, and
// failing a write because an old file could not be deleted would be the
// logger causing the problem it exists to report.
func (r *Rotator) prune() {
	if r.opts.MaxFiles < 0 {
		return
	}

	extension := filepath.Ext(r.opts.Path)
	base := strings.TrimSuffix(filepath.Base(r.opts.Path), extension)
	entries, err := os.ReadDir(filepath.Dir(r.opts.Path))
	if err != nil {
		return
	}

	var rotated []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == filepath.Base(r.opts.Path) {
			continue
		}
		if strings.HasPrefix(name, base+"-") && strings.HasSuffix(name, extension) {
			rotated = append(rotated, name)
		}
	}

	// The names sort chronologically because the timestamp is fixed-width
	// and big-endian, which is the reason for that format.
	sort.Sort(sort.Reverse(sort.StringSlice(rotated)))

	for i, name := range rotated {
		if i < r.opts.MaxFiles {
			continue
		}
		_ = os.Remove(filepath.Join(filepath.Dir(r.opts.Path), name))
	}
}

// fallback writes somewhere else and reports the write as succeeding.
//
// Returning the error instead would propagate it into slog, which discards
// handler errors anyway — so the message would be lost twice over. It is
// reported once, on the fallback itself, and then writes carry on quietly:
// one line per failed write would turn a full disk into a faster full disk.
func (r *Rotator) fallback(p []byte, cause error) (int, error) {
	r.warn(fmt.Errorf("writing to %s: %w", r.opts.Path, cause))
	if n, err := r.opts.Fallback.Write(p); err == nil {
		return n, nil
	}
	return len(p), nil
}

func (r *Rotator) warn(err error) {
	if r.failed {
		return
	}
	r.failed = true
	fmt.Fprintf(r.opts.Fallback,
		"log: %v\nlog: further records go here; this is reported once\n", err)
}

// Close flushes and closes the file.
func (r *Rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
