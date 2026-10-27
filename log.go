// Package log configures slog for a Lemmego application: a readable console,
// a rotating file, or both.
//
// It deliberately does not wrap slog. There is no log.Info — slog.Info is
// the facade, and it already works once this package has set the default
// handler. A second facade would mean two ways to log the same thing, and
// library code calls slog whatever an application prefers, so half the lines
// in a log file would come from the one nobody chose.
//
// What was missing, and what this is:
//
// **A rotating file writer.** Nothing in the standard library has one, so an
// application that wants a log file has to choose between growing it forever
// and finding a third-party writer.
//
// **A console handler that knows whether it is on a terminal.** The
// framework's previous handler colourised unconditionally, so redirecting
// output to a file wrote escape sequences into it — which is the first thing
// anybody trying to keep a log file would hit.
//
// **A fan-out.** Readable lines for whoever is watching and JSON in a file
// for whoever is grepping next week; without it the choice is one or the
// other and the one you did not choose is the one you need.
//
// **One owner of slog.SetDefault.** Two places calling it means the winner
// depends on initialisation order, and the loser's configuration is silently
// discarded.
//
//	logger, err := log.New(log.Options{Channel: "stack"})
//	if err != nil {
//	    return err
//	}
//	log.SetDefault(logger)
//
//	slog.Info("launch published", "slug", launch.Slug)
//
// # Why this module depends on nothing
//
// Standard library only, and that is a constraint rather than a happy
// accident. A logging package is the one thing every other package wants to
// import, framework modules included — so the moment it requires the web
// framework for an app.Provider, two things break at once.
//
// A consumer that imports only this package inherits the whole framework's
// dependency tree: measured before this was split out, 289 modules in the
// build list, 156 of them cloud SDKs, OpenTelemetry and gRPC, for a package
// whose entire job is formatting text. Module graph pruning does not help,
// because the requirements live in go.mod whether or not anything imports
// them.
//
// And `api` could never log through this, because `api -> log -> api` is a
// cycle. That is the direction that should exist: the framework should use
// the logger, not the other way round.
//
// So wiring into an application's lifecycle belongs outside this module — a
// provider that reads configuration, honours "running in console", and calls
// SetDefault once. It is twenty lines, and it is the only part that needs to
// know what an application is.
package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Channel names.
//
// A small closed set, because the value comes from an environment variable
// and a typo in one should name the valid options rather than silently pick a
// default. Composition beyond these is done by building handlers directly —
// see NewFanout.
const (
	// ChannelConsole is readable lines on stderr. The development default.
	ChannelConsole = "console"
	// ChannelFile is JSON in a rotating file.
	ChannelFile = "file"
	// ChannelJSON is JSON on stderr, for a platform that collects stdio.
	ChannelJSON = "json"
	// ChannelStack is console and file together. The production default
	// worth having: the file is the record and the console is what a
	// process manager shows.
	ChannelStack = "stack"
	// ChannelNull discards everything, for a test that asserts on something
	// else.
	ChannelNull = "null"
)

// DefaultPath is where the log goes when nothing says otherwise.
//
// Under storage/ beside the other things an application writes at runtime,
// and in its own directory because rotation puts several files next to the
// active one.
const DefaultPath = "storage/logs/app.log"

// Options configures the logger.
//
// Every field has a working zero value: the zero Options is a console logger
// at info level, which is what a developer wants and what a misconfigured
// deployment should fall back to rather than silence.
type Options struct {
	// Channel is one of the constants above. Empty means ChannelConsole.
	Channel string

	// Level is "debug", "info", "warn" or "error". Empty means info.
	//
	// A string because it comes from configuration. An unrecognised value is
	// info with a warning, not a parse failure: a typo in LOG_LEVEL should
	// not stop the process, and silently choosing error would hide the
	// warning that says so.
	Level string

	// Path is the file for ChannelFile and ChannelStack. Empty means
	// DefaultPath.
	Path string

	// MaxSize, MaxFiles and Daily configure rotation. See RotateOptions.
	MaxSize  int64
	MaxFiles int
	Daily    bool

	// Source adds file:line to each record. Useful in development.
	Source bool

	// Colour overrides terminal detection for the console. Nil detects.
	Colour *bool

	// Console overrides where the console channel writes. Defaults to
	// os.Stderr — stderr rather than stdout so that a command's own output
	// stays separable from its logs, which is what makes `cmd > out.txt`
	// useful.
	Console io.Writer

	// Quiet raises the console to warn while leaving the file alone.
	//
	// For a CLI command, where info-level chatter buries the output somebody
	// ran the command for. The file still gets everything, which is the
	// improvement on simply silencing the logger: a command that fails
	// overnight leaves a record.
	Quiet bool

	// Extra are handlers that receive every record alongside the channel's
	// own, for something that wants the log rather than writes it — a
	// recorder, a metrics counter, a test double.
	//
	// They are folded in for every channel, not only stack. There is no other
	// way to add one: Logger.Handler is read-only, and wrapping it afterwards
	// with a second slog.SetDefault would break the single-owner rule that
	// this package and api/logger are both built on.
	//
	// An Extra handler is held to the same contract as any other: Handle is
	// given a cloned record, so retaining its attrs past the call means
	// copying them out. See Fanout.
	Extra []slog.Handler
}

// Logger is a configured slog.Logger plus whatever has to be closed.
type Logger struct {
	*slog.Logger

	handler slog.Handler
	closers []io.Closer
	path    string
}

// Handler returns the root handler, for a caller composing further.
func (l *Logger) Handler() slog.Handler { return l.handler }

// Path is the active log file, or empty for a channel that writes to none.
//
// Exposed because "where is the log file" is the first question anybody has,
// and an application that answers it at boot saves them finding the
// configuration that decided it.
func (l *Logger) Path() string { return l.path }

// Close closes the file, if there is one.
func (l *Logger) Close() error {
	for _, closer := range l.closers {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Level parses a level name.
//
// The second return is false for an unrecognised name, so a caller can warn
// about a typo rather than acting on a guess.
func Level(name string) (slog.Level, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		return slog.LevelInfo, true
	case "debug":
		return slog.LevelDebug, true
	case "warn", "warning":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// New builds a logger from Options.
//
// It returns an error only for a file it cannot open, which is a
// misconfigured path and worth failing a boot over — a log that silently
// goes nowhere is the failure this package exists to prevent. Everything
// else degrades: an unknown channel is a console with a warning, an unknown
// level is info with a warning.
func New(opts Options) (*Logger, error) {
	level, known := Level(opts.Level)
	if !known {
		defer func() {
			slog.Warn("log: unrecognised level, using info",
				"level", opts.Level, "valid", "debug, info, warn, error")
		}()
	}

	channel := strings.ToLower(strings.TrimSpace(opts.Channel))
	if channel == "" {
		channel = ChannelConsole
	}

	logger := &Logger{}

	switch channel {
	case ChannelNull:
		logger.handler = NewFanout()

	case ChannelConsole:
		logger.handler = logger.console(opts, level)

	case ChannelJSON:
		logger.handler = slog.NewJSONHandler(logger.consoleWriter(opts),
			&slog.HandlerOptions{Level: level, AddSource: opts.Source})

	case ChannelFile:
		handler, err := logger.file(opts, level)
		if err != nil {
			return nil, err
		}
		logger.handler = handler

	case ChannelStack:
		handler, err := logger.file(opts, level)
		if err != nil {
			return nil, err
		}
		logger.handler = NewFanout(logger.console(opts, level), handler)

	default:
		defer func() {
			slog.Warn("log: unrecognised channel, using console",
				"channel", opts.Channel,
				"valid", strings.Join([]string{
					ChannelConsole, ChannelFile, ChannelJSON,
					ChannelStack, ChannelNull,
				}, ", "))
		}()
		logger.handler = logger.console(opts, level)
	}

	// Folded in last, so an Extra handler sees exactly what the channel
	// does. Fanout drops nil members, so a caller need not filter.
	if len(opts.Extra) > 0 {
		logger.handler = NewFanout(append([]slog.Handler{logger.handler}, opts.Extra...)...)
	}

	logger.Logger = slog.New(logger.handler)
	return logger, nil
}

func (l *Logger) consoleWriter(opts Options) io.Writer {
	if opts.Console != nil {
		return opts.Console
	}
	return os.Stderr
}

// console builds the console half, applying Quiet.
func (l *Logger) console(opts Options, level slog.Level) slog.Handler {
	if opts.Quiet && level < slog.LevelWarn {
		level = slog.LevelWarn
	}
	return NewConsoleHandler(l.consoleWriter(opts), ConsoleOptions{
		Level:  level,
		Source: opts.Source,
		Colour: opts.Colour,
	})
}

// file builds the file half.
//
// JSON rather than the console format, and that is the point of having two:
// a file is read by a machine — grep, jq, whatever collects it — and a
// console is read by a person. One format cannot be the best of both, so
// neither half compromises.
func (l *Logger) file(opts Options, level slog.Level) (slog.Handler, error) {
	path := opts.Path
	if strings.TrimSpace(path) == "" {
		path = DefaultPath
	}

	rotator, err := NewRotator(RotateOptions{
		Path:     path,
		MaxSize:  opts.MaxSize,
		MaxFiles: opts.MaxFiles,
		Daily:    opts.Daily,
	})
	if err != nil {
		return nil, fmt.Errorf("log: %w", err)
	}

	l.closers = append(l.closers, rotator)
	l.path = rotator.Path()

	return slog.NewJSONHandler(rotator,
		&slog.HandlerOptions{Level: level, AddSource: opts.Source}), nil
}

// SetDefault installs this logger as slog's default.
//
// One call site for it in an application, which is the whole reason this
// function exists rather than callers reaching for slog.SetDefault: two
// places setting the default means the winner depends on initialisation
// order, and the loser's configuration is silently discarded.
func SetDefault(logger *Logger) { slog.SetDefault(logger.Logger) }
