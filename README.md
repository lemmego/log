# lemmego/log

Configures `slog` for an application: a readable console, a rotating file, or
both.

```go
logger, err := log.New(log.Options{Channel: "stack"})
if err != nil {
    return err
}
log.SetDefault(logger)

slog.Info("launch published", "slug", launch.Slug)
```

## It does not wrap slog

**There is no `log.Info`.** `slog.Info` is the facade and it works the moment
this package has set the default handler. A second facade would mean two ways
to log the same thing — and library code calls `slog` whatever an application
prefers, so half the lines in a log file would come from the one nobody chose.

## Standard library only

A logging package is the one thing every other package wants to import,
framework modules included. If it needed a web framework for an
`app.Provider`, two things would break at once:

- **You would inherit the whole framework's dependency tree** for a package
  whose job is formatting text — around 289 modules, 156 of them cloud SDKs,
  OpenTelemetry and gRPC. Module graph pruning does not help, because the
  requirements are in `go.mod` whether or not anything imports them. This
  module's build list is 2.
- **`api` could never log through it**, because `api → log → api` is a cycle,
  and that is the direction that should exist: the framework should use the
  logger.

So the lifecycle wiring lives outside this module, in `api/logger`. You do not
need to write it — register `&logger.Provider{}` and set the `LOG_*`
variables. It is about twenty lines if you ever want your own: read
configuration, honour "running in console", call `SetDefault` once.

## Install

```bash
go get github.com/lemmego/log
```

## Channels

| | |
|---|---|
| `console` | readable lines on stderr. The development default. |
| `file` | JSON in a rotating file. |
| `json` | JSON on stderr, for a platform that collects stdio. |
| `stack` | console **and** file. The production answer. |
| `null` | discards everything, for a test that asserts on something else. |

A small closed set, because the value comes from an environment variable and a
typo should name the valid options rather than silently pick a default.
Composition beyond these is done by building handlers directly — see
`NewFanout`.

**`stack` is the production default worth having.** A container platform
collects stdio, so a deployment whose only log is a file in an ephemeral
filesystem has a log nobody will read; and a deployment whose only log is
stdio has nothing to grep next week. Both is the answer that works in both
places.

The two halves use **different formats**, because a file is read by a
machine — `grep`, `jq`, whatever collects it — and a console is read by a
person. One format cannot be the best of both, so neither half compromises.

## Options

```go
log.Options{
    Channel:  "stack",
    Level:    "debug",              // debug | info | warn | error
    Path:     "storage/logs/app.log",
    MaxSize:  32 << 20,
    MaxFiles: 7,
    Daily:    true,
    Source:   true,
    Quiet:    false,
    Console:  os.Stderr,
}
```

**Every field has a working zero value.** The zero `Options` is a console
logger at info level — which is what a developer wants and what a
misconfigured deployment should fall back to rather than silence.

`New` returns an error **only** for a file it cannot open. That is a
misconfigured path and worth failing a boot over: a log that silently goes
nowhere is the failure this package exists to prevent. Everything else
degrades loudly — an unknown channel is a console with a warning, an unknown
level is info with a warning.

`Quiet` raises the **console** to warn and leaves the file alone. For a CLI
command, where info chatter buries the output somebody ran the command for —
and the improvement on simply silencing the logger is that a command which
fails overnight still leaves a record.

## Colour

```go
log.Colour(w)   // is this writer a terminal?
```

Three conditions, and all of them matter. The writer has to be a **character
device**; `NO_COLOR` is honoured; `TERM=dumb` is honoured.

The framework's previous handler colourised **unconditionally**, so
`make run > app.log` wrote `\033[36m` into the file. That is the first thing
anybody trying to keep a log file would have hit, and it is why this check
exists rather than being assumed.

## The console format

```
03:04:36.950 INFO  log: writing to file path=storage/logs/app.log channel=stack
03:04:36.962 DEBUG cache: ready driver=redis codec=json prefix=lemmego_cache:
```

One line per record, not the indented JSON the previous handler produced: a
request that logs four times should be four lines, not forty, and `grep` on a
multi-line record returns the line without its context. Levels are padded to
five so the messages line up, which is most of what makes a log skimmable.

`WithAttrs` and `WithGroup` behave correctly — groups qualify keys
(`db.query.duration`), `LogValuer` is resolved before formatting so a
redacting type actually redacts, an empty `Attr` is dropped as `slog`
documents, and values containing a space, a quote or an `=` are quoted.

**A record is one `Write`.** Both halves matter: a mutex keeps two goroutines
from interleaving, and a single write keeps a record atomic against another
process appending to the same file.

## Rotation

```go
log.RotateOptions{
    Path:     "storage/logs/app.log",
    MaxSize:  32 << 20,   // negative disables
    Daily:    true,
    MaxFiles: 7,          // negative keeps everything
}
```

Nothing in the standard library rotates, so an application that wants a log
file otherwise chooses between growing it forever and finding a third-party
writer.

**Size and date, not one or the other.** A size limit stops the disk filling;
a date in the filename is what makes a file findable. Laravel's daily channel
does only the second, so a bad day can still write forty gigabytes.

**Rotation happens before the limit is exceeded**, so `MaxSize` is a ceiling
rather than something the file is already over by the time it is noticed.

Rotated files are named `app-2026-10-12-030458.log` rather than `.1`, `.2`:
renaming N files on every rotation is N syscalls that can fail halfway and
leave a gap, and `.1` tells you nothing about when it was. The timestamp is
fixed-width and big-endian so the names sort chronologically, which is what
pruning relies on.

**Files are `0600` in a `0700` directory.** Not fussiness: a log holds
whatever was logged — a verification link, a reset token, a customer's
address, a webhook body. Laravel ships `0664` and every guide tells you to
loosen it so the web server can read it, which is how a log ends up
world-readable on a shared host.

### When writing fails

A logger that **panics** turns a full disk into an outage. A logger that
**drops silently** is worse than no logger. So a failed write goes to the
fallback — `os.Stderr` by default, where a process manager captures it — and
says so **once**: one line per failed write would turn a full disk into a
faster full disk.

`NewRotator` *does* error for a path it cannot open at all, because that is a
misconfiguration at boot rather than a failure under load.

## `Fanout`

```go
log.NewFanout(consoleHandler, fileHandler)
```

`Enabled` is **any member, not all**: a file at debug behind a console at info
has to see the debug records, and "all" would let the quietest member set the
level for everything.

`Handle` delivers to **every** member even after one fails — a file that has
filled must not stop the console from showing the error that says so, which is
exactly when both are needed. Each member gets `record.Clone()`, because
`Record.Attrs` is single-pass over a shared backing array and handing one
`Record` to two handlers is a data race.

## Testing

```bash
go test ./...
go test -race ./...
```

34 tests, no database and no network. The ones worth knowing about: writes
from sixteen goroutines produce whole lines and not spliced ones; a closed
descriptor falls back and reports once; the active file never exceeds
`MaxSize`; pruning keeps `MaxFiles`; a regular file is not treated as a
terminal and no escape sequences reach it; a `LogValuer` is resolved; `Quiet`
raises the console and leaves the file alone.

A test that wants silence uses `Channel: "null"`. A test that wants to assert
on output passes `Console:` a buffer and `Colour:` a pointer to false.
