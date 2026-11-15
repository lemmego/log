package log

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// swapStreams points the split console at buffers for the duration of a test.
func swapStreams(t *testing.T) (out, errOut *bytes.Buffer) {
	t.Helper()
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	oldOut, oldErr := stdoutStream, stderrStream
	stdoutStream, stderrStream = out, errOut
	t.Cleanup(func() { stdoutStream, stderrStream = oldOut, oldErr })
	return out, errOut
}

// A platform that reads stderr as "this is an error" reports every info line
// as one unless the levels are divided between the streams. Both halves are
// asserted: it is not enough that info reaches stdout if it also reaches
// stderr, because then the severity is still wrong.
func TestSplitStreamsSendsInfoToStdoutAndErrorsToStderr(t *testing.T) {
	out, errOut := swapStreams(t)

	no := false
	logger := &Logger{}
	handler := logger.console(Options{SplitStreams: true, Colour: &no}, slog.LevelDebug)
	log := slog.New(handler)

	log.Info("a routine thing")
	log.Error("a real failure")

	if !strings.Contains(out.String(), "a routine thing") {
		t.Errorf("info did not reach stdout: %q", out.String())
	}
	if strings.Contains(errOut.String(), "a routine thing") {
		t.Errorf("info also reached stderr, so it is still reported as an error: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "a real failure") {
		t.Errorf("error did not reach stderr: %q", errOut.String())
	}
	if strings.Contains(out.String(), "a real failure") {
		t.Errorf("error also reached stdout: %q", out.String())
	}
}

// The default is unchanged, which is what keeps `cmd > out.txt` useful for a
// CLI command.
func TestWithoutSplitStreamsEverythingStaysOnOneWriter(t *testing.T) {
	out, errOut := swapStreams(t)

	buf := &bytes.Buffer{}
	no := false
	logger := &Logger{}
	handler := logger.console(Options{Console: buf, Colour: &no}, slog.LevelDebug)
	log := slog.New(handler)

	log.Info("a routine thing")
	log.Error("a real failure")

	if !strings.Contains(buf.String(), "a routine thing") || !strings.Contains(buf.String(), "a real failure") {
		t.Errorf("the configured writer did not get both records: %q", buf.String())
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("a configured Console writer was split anyway: stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

// Quiet leaves the lower half with nothing to write, so there is no handler
// for it rather than one that can never be Enabled.
func TestSplitStreamsUnderQuietWritesOnlyToStderr(t *testing.T) {
	out, errOut := swapStreams(t)

	no := false
	logger := &Logger{}
	handler := logger.console(Options{SplitStreams: true, Quiet: true, Colour: &no}, slog.LevelDebug)
	log := slog.New(handler)

	log.Info("a routine thing")
	log.Warn("worth knowing")

	if out.Len() != 0 {
		t.Errorf("Quiet still wrote to stdout: %q", out.String())
	}
	if strings.Contains(errOut.String(), "a routine thing") {
		t.Errorf("Quiet did not raise the console to warn: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "worth knowing") {
		t.Errorf("warn did not reach stderr: %q", errOut.String())
	}
}

// MaxLevel is a half-open bound; without it a minimum alone cannot express
// "info and below".
func TestMaxLevelBoundsAConsoleHandlerFromAbove(t *testing.T) {
	buf := &bytes.Buffer{}
	no := false
	h := NewConsoleHandler(buf, ConsoleOptions{
		Level:    slog.LevelDebug,
		MaxLevel: slog.LevelWarn,
		Colour:   &no,
	})

	for _, c := range []struct {
		level slog.Level
		want  bool
	}{
		{slog.LevelDebug, true},
		{slog.LevelInfo, true},
		{slog.LevelWarn, false},
		{slog.LevelError, false},
	} {
		if got := h.Enabled(context.Background(), c.level); got != c.want {
			t.Errorf("Enabled(%v) = %v, want %v", c.level, got, c.want)
		}
	}
}
