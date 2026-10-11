package log

import (
	"context"
	"errors"
	"log/slog"
)

// Fanout sends every record to several handlers.
//
// This is the "stack" a real deployment wants: readable lines on stdout for
// whoever is watching, and JSON in a file for whoever is grepping next week.
// Without it the choice is one or the other, and the one you did not choose
// is the one you need.
type Fanout struct {
	handlers []slog.Handler
}

// NewFanout returns a handler writing to all of these.
func NewFanout(handlers ...slog.Handler) *Fanout {
	kept := make([]slog.Handler, 0, len(handlers))
	for _, handler := range handlers {
		if handler != nil {
			kept = append(kept, handler)
		}
	}
	return &Fanout{handlers: kept}
}

// Enabled is true when any member is.
//
// Any rather than all, because the members have different levels on purpose:
// a file at debug behind a console at info has to see the debug records, and
// an "all" would mean the quietest handler silently sets the level for every
// other one.
func (f *Fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range f.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle delivers to every member that wants the record.
//
// Every member, even after one fails. A file that has filled must not stop
// the console from showing the error that says so — which is exactly when
// both are needed and exactly when a loop that returned early would deliver
// neither.
func (f *Fanout) Handle(ctx context.Context, record slog.Record) error {
	var failures []error
	for _, handler := range f.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		// A clone per handler: Record.Attrs is single-pass over a shared
		// backing array, and handing the same Record to two handlers that
		// both iterate it is a data race on a growing slice.
		if err := handler.Handle(ctx, record.Clone()); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (f *Fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return f
	}
	derived := make([]slog.Handler, 0, len(f.handlers))
	for _, handler := range f.handlers {
		derived = append(derived, handler.WithAttrs(attrs))
	}
	return &Fanout{handlers: derived}
}

func (f *Fanout) WithGroup(name string) slog.Handler {
	if name == "" {
		return f
	}
	derived := make([]slog.Handler, 0, len(f.handlers))
	for _, handler := range f.handlers {
		derived = append(derived, handler.WithGroup(name))
	}
	return &Fanout{handlers: derived}
}
