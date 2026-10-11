package log

import "runtime"

// frame is the part of a runtime.Frame this package renders.
type frame struct {
	File string
	Line int
}

// runtimeCallersFrames resolves one program counter.
//
// Its own file because slog stores a PC rather than a resolved frame — the
// resolution is the expensive half and is skipped entirely for a record
// nobody formats.
func runtimeCallersFrames(pc uintptr) frame {
	frames := runtime.CallersFrames([]uintptr{pc})
	resolved, _ := frames.Next()
	return frame{File: resolved.File, Line: resolved.Line}
}
