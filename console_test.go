package log

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// countingWriter records how many Write calls arrive, so a test can assert a
// record is emitted in one.
type countingWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	writes int
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	return w.buffer.Write(p)
}

func (w *countingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func console(w *countingWriter) *slog.Logger {
	off := false
	return slog.New(NewConsoleHandler(w, ConsoleOptions{
		Level: slog.LevelDebug, Colour: &off,
	}))
}

// The failure that made the previous handler unusable for a log file:
// colourising with no terminal check writes escape sequences into whatever
// the output is redirected to.
func TestColourIsOffWhenTheDestinationIsNotATerminal(t *testing.T) {
	if Colour(&bytes.Buffer{}) {
		t.Fatal("a buffer was treated as a terminal")
	}

	path := filepath.Join(t.TempDir(), "app.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	defer file.Close()
	if Colour(file) {
		t.Fatal("a regular file was treated as a terminal")
	}

	// And the handler emits no escapes into it.
	logger := slog.New(NewConsoleHandler(file, ConsoleOptions{Level: slog.LevelInfo}))
	logger.Info("a message", "key", "value")
	contents, _ := os.ReadFile(path)
	if bytes.Contains(contents, []byte("\033[")) {
		t.Fatalf("escape sequences were written to a file: %q", contents)
	}
}

func TestNoColorAndDumbTerminalAreHonoured(t *testing.T) {
	// A pipe is not a character device either, so these assert the
	// environment checks rather than the device check.
	t.Setenv("NO_COLOR", "1")
	if Colour(os.Stdout) {
		t.Fatal("NO_COLOR was ignored")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if Colour(os.Stdout) {
		t.Fatal("TERM=dumb was ignored")
	}
}

// A record is one Write, which is what keeps it atomic against another
// process appending to the same file.
func TestARecordIsASingleWrite(t *testing.T) {
	writer := &countingWriter{}
	console(writer).Info("a message", "a", 1, "b", "two", "c", true)

	if writer.writes != 1 {
		t.Fatalf("the record took %d writes, want 1", writer.writes)
	}
	if lines := strings.Count(writer.String(), "\n"); lines != 1 {
		t.Fatalf("the record spans %d lines, want 1", lines)
	}
}

func TestAttributesAreRenderedAsKeyValues(t *testing.T) {
	writer := &countingWriter{}
	console(writer).Info("published", "slug", "sidekick", "upvotes", 42)

	got := writer.String()
	for _, want := range []string{"published", "slug=sidekick", "upvotes=42"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

// A value with a space would otherwise run into the next key and make the
// line unparseable by eye or by cut.
func TestValuesNeedingQuotesGetThem(t *testing.T) {
	writer := &countingWriter{}
	console(writer).Info("msg", "headline", "Building tools", "empty", "", "eq", "a=b")

	got := writer.String()
	if !strings.Contains(got, `headline="Building tools"`) {
		t.Fatalf("a value with a space was not quoted: %q", got)
	}
	if !strings.Contains(got, `empty=""`) {
		t.Fatalf("an empty value was not marked: %q", got)
	}
	if !strings.Contains(got, `eq="a=b"`) {
		t.Fatalf("a value containing = was not quoted: %q", got)
	}
}

// WithAttrs and WithGroup are where hand-written slog handlers usually break.
func TestWithAttrsAndWithGroupQualifyKeys(t *testing.T) {
	writer := &countingWriter{}
	logger := console(writer).
		With("request_id", "abc").
		WithGroup("db").
		With("driver", "postgres")

	logger.Info("query ran", "duration_ms", 12)

	got := writer.String()
	for _, want := range []string{
		"request_id=abc",     // before the group: unqualified
		"db.driver=postgres", // added inside the group
		"db.duration_ms=12",  // on the record, inside the group
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestNestedGroupsNest(t *testing.T) {
	writer := &countingWriter{}
	console(writer).WithGroup("outer").WithGroup("inner").Info("msg", "key", "v")

	if got := writer.String(); !strings.Contains(got, "outer.inner.key=v") {
		t.Fatalf("groups did not nest: %q", got)
	}
}

func TestAnInlineGroupAttrIsExpanded(t *testing.T) {
	writer := &countingWriter{}
	console(writer).Info("msg", slog.Group("http", "status", 200, "method", "GET"))

	got := writer.String()
	if !strings.Contains(got, "http.status=200") || !strings.Contains(got, "http.method=GET") {
		t.Fatalf("a group attribute was not expanded: %q", got)
	}
}

// secret is a LogValuer, which is the mechanism for keeping a credential out
// of a log. Formatting the wrapper instead would print a struct and defeat it.
type secret struct{ value string }

func (s secret) LogValue() slog.Value { return slog.StringValue("[redacted]") }

func TestLogValuerIsResolved(t *testing.T) {
	writer := &countingWriter{}
	console(writer).Info("msg", "token", secret{value: "hunter2"})

	got := writer.String()
	if strings.Contains(got, "hunter2") {
		t.Fatalf("a LogValuer was not resolved, so the real value was logged: %q", got)
	}
	if !strings.Contains(got, "token=[redacted]") {
		t.Fatalf("the resolved value is missing: %q", got)
	}
}

// slog documents an empty Attr as the way to drop one.
func TestAnEmptyAttrIsDropped(t *testing.T) {
	writer := &countingWriter{}
	slog.New(NewConsoleHandler(writer, ConsoleOptions{
		Level:  slog.LevelDebug,
		Colour: func() *bool { off := false; return &off }(),
	})).Info("msg", slog.Attr{})

	if got := writer.String(); strings.Contains(got, "=") {
		t.Fatalf("an empty attr produced output: %q", got)
	}
}

func TestTheLevelIsRespected(t *testing.T) {
	writer := &countingWriter{}
	off := false
	logger := slog.New(NewConsoleHandler(writer, ConsoleOptions{
		Level: slog.LevelWarn, Colour: &off,
	}))

	logger.Debug("debug")
	logger.Info("info")
	logger.Warn("warn")
	logger.Error("error")

	got := writer.String()
	if strings.Contains(got, "debug") || strings.Contains(got, "info") {
		t.Fatalf("records below the level were emitted: %q", got)
	}
	if !strings.Contains(got, "warn") || !strings.Contains(got, "error") {
		t.Fatalf("records at or above the level were dropped: %q", got)
	}
}

// Two loggers derived from one handler share its mutex, or a line from one
// interleaves with a line from the other.
func TestDerivedLoggersSerialiseAgainstEachOther(t *testing.T) {
	writer := &countingWriter{}
	root := console(writer)
	derived := root.With("derived", true)

	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 25 {
				root.Info("from the root", "pad", strings.Repeat("a", 30))
				derived.Info("from the derived", "pad", strings.Repeat("b", 30))
			}
		}()
	}
	wait.Wait()

	lines := strings.Split(strings.TrimSuffix(writer.String(), "\n"), "\n")
	if len(lines) != 8*25*2 {
		t.Fatalf("got %d lines, want %d", len(lines), 8*25*2)
	}
	for i, line := range lines {
		// Every line carries exactly one message, so none was spliced.
		roots := strings.Count(line, "from the root")
		derivs := strings.Count(line, "from the derived")
		if roots+derivs != 1 {
			t.Fatalf("line %d holds %d messages: %q", i, roots+derivs, line)
		}
	}
}

func TestSourceAddsFileAndLine(t *testing.T) {
	writer := &countingWriter{}
	off := false
	slog.New(NewConsoleHandler(writer, ConsoleOptions{
		Level: slog.LevelInfo, Source: true, Colour: &off,
	})).Info("msg")

	if got := writer.String(); !strings.Contains(got, "console_test.go:") {
		t.Fatalf("no source location: %q", got)
	}
}

func TestEnabledIsCheckedBeforeFormatting(t *testing.T) {
	handler := NewConsoleHandler(&countingWriter{}, ConsoleOptions{Level: slog.LevelWarn})
	if handler.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info is enabled on a warn handler")
	}
	if !handler.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("error is not enabled on a warn handler")
	}
}
