package log

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingHandler captures what it was given, and can be made to fail.
type recordingHandler struct {
	mu       sync.Mutex
	level    slog.Level
	messages []string
	attrs    []string
	fail     error
}

func (h *recordingHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, record.Message)
	record.Attrs(func(attr slog.Attr) bool {
		h.attrs = append(h.attrs, attr.Key+"="+attr.Value.String())
		return true
	})
	return h.fail
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) seen() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.messages...)
}

func TestLevelParsing(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"":        slog.LevelInfo,
		"info":    slog.LevelInfo,
		"DEBUG":   slog.LevelDebug,
		" warn ":  slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
	} {
		got, known := Level(name)
		if !known {
			t.Errorf("Level(%q) reported unknown", name)
		}
		if got != want {
			t.Errorf("Level(%q) = %v, want %v", name, got, want)
		}
	}

	// An unrecognised level is info and says so, rather than being a parse
	// failure that stops the process or a silent guess at error.
	got, known := Level("verbose")
	if known {
		t.Fatal("an unrecognised level was reported as known")
	}
	if got != slog.LevelInfo {
		t.Fatalf("an unrecognised level fell back to %v, want info", got)
	}
}

func TestFanoutDeliversToEveryHandler(t *testing.T) {
	first := &recordingHandler{level: slog.LevelDebug}
	second := &recordingHandler{level: slog.LevelDebug}

	slog.New(NewFanout(first, second)).Info("once", "key", "value")

	if len(first.seen()) != 1 || len(second.seen()) != 1 {
		t.Fatalf("delivery was %d and %d, want 1 each",
			len(first.seen()), len(second.seen()))
	}
	if first.attrs[0] != "key=value" || second.attrs[0] != "key=value" {
		t.Fatalf("attributes did not reach both: %v / %v", first.attrs, second.attrs)
	}
}

// A file that has filled must not stop the console from showing the error
// that says so — which is exactly when both are needed.
func TestOneFailingHandlerDoesNotStopTheOthers(t *testing.T) {
	broken := &recordingHandler{level: slog.LevelDebug, fail: errors.New("disk full")}
	working := &recordingHandler{level: slog.LevelDebug}

	fanout := NewFanout(broken, working)
	record := slog.NewRecord(time.Now(), slog.LevelError, "the disk is full", 0)

	err := fanout.Handle(context.Background(), record)
	if err == nil {
		t.Fatal("the failure was swallowed")
	}
	if len(working.seen()) != 1 {
		t.Fatal("the working handler did not get the record")
	}
}

// Any rather than all: a file at debug behind a console at info has to see
// the debug records, and "all" would let the quietest member set the level
// for every other one.
func TestFanoutEnabledIsAnyNotAll(t *testing.T) {
	quiet := &recordingHandler{level: slog.LevelError}
	verbose := &recordingHandler{level: slog.LevelDebug}

	fanout := NewFanout(quiet, verbose)
	if !fanout.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("a debug record was refused although one member wants it")
	}

	// And only the member that wants it gets it.
	slog.New(fanout).Debug("quiet please")
	if len(quiet.seen()) != 0 {
		t.Fatal("a handler received a record below its level")
	}
	if len(verbose.seen()) != 1 {
		t.Fatal("the verbose handler missed a record")
	}
}

// Record.Attrs is single-pass over a shared backing array, so two handlers
// iterating the same Record is a race. Run this with -race.
func TestFanoutDoesNotShareARecordBetweenHandlers(t *testing.T) {
	handlers := make([]slog.Handler, 0, 4)
	for range 4 {
		handlers = append(handlers, &recordingHandler{level: slog.LevelDebug})
	}
	logger := slog.New(NewFanout(handlers...))

	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 50 {
				logger.Info("concurrent", "a", 1, "b", 2, "c", 3)
			}
		}()
	}
	wait.Wait()

	for i, handler := range handlers {
		if got := len(handler.(*recordingHandler).seen()); got != 400 {
			t.Fatalf("handler %d saw %d records, want 400", i, got)
		}
	}
}

func TestWithAttrsAndWithGroupReachEveryMember(t *testing.T) {
	first := &recordingHandler{level: slog.LevelDebug}
	second := &recordingHandler{level: slog.LevelDebug}

	fanout := NewFanout(first, second)
	if derived := fanout.WithAttrs(nil); derived != fanout {
		t.Fatal("WithAttrs(nil) allocated a new handler")
	}
	if derived := fanout.WithGroup(""); derived != fanout {
		t.Fatal(`WithGroup("") allocated a new handler`)
	}

	slog.New(fanout).With("shared", true).WithGroup("g").Info("msg")
	if len(first.seen()) != 1 || len(second.seen()) != 1 {
		t.Fatal("a derived fanout did not deliver to both")
	}
}

func TestTheNullChannelDiscards(t *testing.T) {
	logger, err := New(Options{Channel: ChannelNull})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Nothing to assert against except that it does not panic and writes
	// nowhere — which is the whole contract for a test that wants silence.
	logger.Error("this goes nowhere")
	if logger.Path() != "" {
		t.Fatalf("the null channel opened a file at %q", logger.Path())
	}
}

func TestTheFileChannelWritesJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")

	logger, err := New(Options{Channel: ChannelFile, Path: path, Level: "debug"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("structured", "slug", "sidekick")
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	line := string(contents)
	// JSON, because a file is read by a machine and a console by a person.
	for _, want := range []string{`"msg":"structured"`, `"slug":"sidekick"`, `"level":"INFO"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %s in %q", want, line)
		}
	}
	if logger.Path() != path {
		t.Fatalf("Path() = %q, want %q", logger.Path(), path)
	}
}

func TestTheStackChannelWritesBoth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	writer := &countingWriter{}
	off := false

	logger, err := New(Options{
		Channel: ChannelStack, Path: path, Level: "debug",
		Console: writer, Colour: &off,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("to both places", "n", 1)
	logger.Close()

	if got := writer.String(); !strings.Contains(got, "to both places") {
		t.Fatalf("the console half missed it: %q", got)
	}
	contents, _ := os.ReadFile(path)
	if !strings.Contains(string(contents), "to both places") {
		t.Fatalf("the file half missed it: %q", contents)
	}
}

// Quiet is for a CLI command: the console stops burying the output somebody
// ran the command for, and the file still gets everything — so a command
// that fails overnight leaves a record.
func TestQuietRaisesTheConsoleAndLeavesTheFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	writer := &countingWriter{}
	off := false

	logger, err := New(Options{
		Channel: ChannelStack, Path: path, Level: "debug", Quiet: true,
		Console: writer, Colour: &off,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("chatter")
	logger.Warn("worth seeing")
	logger.Close()

	seen := writer.String()
	if strings.Contains(seen, "chatter") {
		t.Fatalf("Quiet did not raise the console: %q", seen)
	}
	if !strings.Contains(seen, "worth seeing") {
		t.Fatalf("Quiet silenced a warning: %q", seen)
	}

	contents, _ := os.ReadFile(path)
	if !strings.Contains(string(contents), "chatter") {
		t.Fatalf("Quiet also quietened the file, which it must not: %q", contents)
	}
}

// An unknown channel is a console with a warning, not silence: a typo in
// LOG_CHANNEL must not be the reason there are no logs.
func TestAnUnknownChannelFallsBackToTheConsole(t *testing.T) {
	writer := &countingWriter{}
	off := false

	logger, err := New(Options{Channel: "sylog", Console: writer, Colour: &off})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("still logged")

	if got := writer.String(); !strings.Contains(got, "still logged") {
		t.Fatalf("a typo in the channel produced silence: %q", got)
	}
}

// A file that cannot be opened fails the boot, because a deployment is better
// off refusing to start than running unobserved.
func TestAnUnopenableFileChannelIsAnError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("setting up: %v", err)
	}

	if _, err := New(Options{Channel: ChannelFile, Path: filepath.Join(blocker, "app.log")}); err == nil {
		t.Fatal("an unopenable file channel was accepted")
	}
}

// The zero Options is a working console logger, which is what a misconfigured
// deployment should fall back to rather than silence.
func TestTheZeroOptionsWork(t *testing.T) {
	logger, err := New(Options{})
	if err != nil {
		t.Fatalf("New(Options{}): %v", err)
	}
	if logger.Logger == nil {
		t.Fatal("no logger")
	}
	if logger.Handler() == nil {
		t.Fatal("no handler")
	}
}

// capturing is a handler that keeps what it is given.
type capturing struct {
	mu      sync.Mutex
	records []slog.Record
	attrs   [][]slog.Attr
}

func (c *capturing) Enabled(context.Context, slog.Level) bool { return true }

func (c *capturing) Handle(_ context.Context, record slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, record)

	// Copied out rather than retained. Record.Attrs is single-pass over a
	// shared backing array, which is why Fanout clones per handler — a
	// handler that holds the record and iterates later races with whoever
	// else was given it.
	var attrs []slog.Attr
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	c.attrs = append(c.attrs, attrs)
	return nil
}

func (c *capturing) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capturing) WithGroup(string) slog.Handler      { return c }

func (c *capturing) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records)
}

// TestExtraHandlersReceiveEveryChannel.
//
// Only ChannelStack built a Fanout before, so there was nowhere to add a
// handler on the channels most deployments actually use — and a consumer
// would have reached around with a second slog.SetDefault, which breaks the
// single-owner rule this package and api/logger are both built on.
func TestExtraHandlersReceiveEveryChannel(t *testing.T) {
	for _, channel := range []string{
		ChannelConsole, ChannelJSON, ChannelFile, ChannelStack,
	} {
		t.Run(channel, func(t *testing.T) {
			extra := &capturing{}
			logger, err := New(Options{
				Channel: channel,
				Level:   "debug",
				Path:    filepath.Join(t.TempDir(), "app.log"),
				Console: io.Discard,
				Extra:   []slog.Handler{extra},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = logger.Close() })

			logger.Info("hello", "who", "world")

			if got := extra.count(); got != 1 {
				t.Errorf("the extra handler received %d records on channel %s, want 1",
					got, channel)
			}
		})
	}
}

// TestExtraHandlersDoNotReplaceTheChannel, because a recorder that silently
// swallowed the log file would be the worst possible trade.
func TestExtraHandlersDoNotReplaceTheChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	extra := &capturing{}

	logger, err := New(Options{
		Channel: ChannelFile,
		Level:   "debug",
		Path:    path,
		Extra:   []slog.Handler{extra},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	logger.Info("written to both")
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log file: %v", err)
	}
	if !strings.Contains(string(contents), "written to both") {
		t.Error("the record did not reach the file; an extra handler replaced " +
			"the channel rather than joining it")
	}
	if extra.count() != 1 {
		t.Error("the record did not reach the extra handler")
	}
}

// TestNoExtraHandlersLeavesTheHandlerAlone, so the common case gains no
// wrapper and no allocation.
func TestNoExtraHandlersLeavesTheHandlerAlone(t *testing.T) {
	logger, err := New(Options{
		Channel: ChannelConsole,
		Console: io.Discard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })

	if _, wrapped := logger.Handler().(*Fanout); wrapped {
		t.Error("the console channel was wrapped in a Fanout with no extra handlers")
	}
}
