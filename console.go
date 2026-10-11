package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ANSI codes, used only when the destination is a terminal.
const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiCyan   = "\033[36m"
	ansiBold   = "\033[1m"
)

// Colour reports whether output to this writer should be colourised.
//
// Three conditions, and all of them matter.
//
// The writer has to be a character device. The framework's previous handler
// colourised unconditionally, so `make run > app.log` wrote escape sequences
// into the file — which is the first thing anybody trying to keep a log file
// would have hit.
//
// NO_COLOR is honoured, because it is the convention and somebody setting it
// has a reason — a screen reader, a dumb pager, a CI log viewer.
//
// TERM=dumb is honoured for the same reason, and because Emacs's shell sets
// it.
func Colour(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// ConsoleHandler writes one line per record, for a person reading along.
//
// One line and not the indented JSON the framework's previous handler
// produced: a request that logs four times should be four lines, not forty,
// and `grep` on a multi-line record returns the line without the context.
type ConsoleHandler struct {
	mu    *sync.Mutex
	out   io.Writer
	level slog.Leveler

	colour bool
	source bool

	// groups is the open group path, and attrs is everything WithAttrs has
	// contributed, already formatted. Pre-formatting is what makes a logger
	// built with With() cheap to use in a loop: the attrs are rendered once
	// when the logger is derived rather than on every record.
	groups []string
	attrs  string
}

// ConsoleOptions configures a ConsoleHandler.
type ConsoleOptions struct {
	Level slog.Leveler
	// Source adds the file and line. Useful in development and noisy in
	// production, where the message and its attributes are what you search.
	Source bool
	// Colour overrides the terminal detection. Nil means detect.
	Colour *bool
}

// NewConsoleHandler returns a handler writing to w.
func NewConsoleHandler(w io.Writer, opts ConsoleOptions) *ConsoleHandler {
	if w == nil {
		w = os.Stderr
	}
	if opts.Level == nil {
		opts.Level = slog.LevelInfo
	}

	colour := Colour(w)
	if opts.Colour != nil {
		colour = *opts.Colour
	}

	return &ConsoleHandler{
		mu:     &sync.Mutex{},
		out:    w,
		level:  opts.Level,
		colour: colour,
		source: opts.Source,
	}
}

func (h *ConsoleHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

// WithAttrs returns a handler with these attributes on every record.
//
// The mutex is shared with the parent rather than copied: two handlers
// derived from one writer must serialise against each other, or a line from
// a derived logger interleaves with a line from its parent.
func (h *ConsoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := *h
	builder := &strings.Builder{}
	builder.WriteString(h.attrs)
	for _, attr := range attrs {
		h.appendAttr(builder, attr, h.groups)
	}
	clone.attrs = builder.String()
	return &clone
}

func (h *ConsoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.groups = append(append([]string{}, h.groups...), name)
	return &clone
}

func (h *ConsoleHandler) Handle(_ context.Context, record slog.Record) error {
	line := &strings.Builder{}

	stamp := record.Time
	if stamp.IsZero() {
		stamp = time.Now()
	}
	h.paint(line, ansiDim, stamp.Format("15:04:05.000"))
	line.WriteByte(' ')

	h.paint(line, h.levelColour(record.Level), h.levelLabel(record.Level))
	line.WriteByte(' ')

	h.paint(line, ansiBold, record.Message)

	if h.attrs != "" {
		line.WriteString(h.attrs)
	}
	record.Attrs(func(attr slog.Attr) bool {
		h.appendAttr(line, attr, h.groups)
		return true
	})

	if h.source && record.PC != 0 {
		if source := caller(record.PC); source != "" {
			line.WriteByte(' ')
			h.paint(line, ansiDim, source)
		}
	}

	line.WriteByte('\n')

	// One Write per record, under the lock. Both halves matter: the lock
	// keeps two goroutines from interleaving, and a single write keeps a
	// record atomic against another process appending to the same file.
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, line.String())
	return err
}

// levelLabel pads to five so the messages line up in a column, which is
// most of what makes a log skimmable.
func (h *ConsoleHandler) levelLabel(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "DEBUG"
	case level < slog.LevelWarn:
		return "INFO "
	case level < slog.LevelError:
		return "WARN "
	default:
		return "ERROR"
	}
}

func (h *ConsoleHandler) levelColour(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return ansiDim
	case level < slog.LevelWarn:
		return ansiCyan
	case level < slog.LevelError:
		return ansiYellow
	default:
		return ansiRed
	}
}

func (h *ConsoleHandler) paint(builder *strings.Builder, colour, text string) {
	if !h.colour {
		builder.WriteString(text)
		return
	}
	builder.WriteString(colour)
	builder.WriteString(text)
	builder.WriteString(ansiReset)
}

// appendAttr formats one attribute, recursing into groups.
func (h *ConsoleHandler) appendAttr(builder *strings.Builder, attr slog.Attr, groups []string) {
	// Resolve first: a LogValuer's value is what the caller meant to log,
	// and formatting the wrapper instead prints a struct address.
	attr.Value = attr.Value.Resolve()

	// An empty Attr is slog's documented way to drop one.
	if attr.Equal(slog.Attr{}) {
		return
	}

	if attr.Value.Kind() == slog.KindGroup {
		nested := attr.Value.Group()
		if len(nested) == 0 {
			return
		}
		inner := groups
		if attr.Key != "" {
			inner = append(append([]string{}, groups...), attr.Key)
		}
		for _, one := range nested {
			h.appendAttr(builder, one, inner)
		}
		return
	}

	builder.WriteByte(' ')
	h.paint(builder, ansiGreen, qualify(attr.Key, groups))
	builder.WriteByte('=')
	builder.WriteString(quoteIfNeeded(attr.Value.String()))
}

// qualify prefixes a key with its open groups: "db.query.duration".
func qualify(key string, groups []string) string {
	if len(groups) == 0 {
		return key
	}
	return strings.Join(groups, ".") + "." + key
}

// quoteIfNeeded quotes a value containing whitespace, a quote, or an equals
// sign, so a line stays parseable by eye and by cut.
func quoteIfNeeded(value string) string {
	if value == "" {
		return `""`
	}
	if strings.ContainsAny(value, " \t\n\r\"=") {
		return strconv.Quote(value)
	}
	return value
}

// caller renders the file:line for a program counter.
func caller(pc uintptr) string {
	frames := runtimeCallersFrames(pc)
	if frames.File == "" {
		return ""
	}
	// Two trailing path segments: the full path is mostly the machine the
	// binary was built on, and the base alone is ambiguous across packages.
	file := frames.File
	if cut := strings.LastIndexByte(file, '/'); cut >= 0 {
		if prior := strings.LastIndexByte(file[:cut], '/'); prior >= 0 {
			file = file[prior+1:]
		}
	}
	return fmt.Sprintf("%s:%d", file, frames.Line)
}
