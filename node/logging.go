package node

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// Log levels, and the option of machine-readable output.
//
// Everything a node had to say went through `log.Printf` at one volume: every
// accepted block, every peer connect and disconnect, every dropped transaction,
// with no way to quieten a busy node or to turn up detail on a specific problem.
// On a chain producing a block every five seconds that is a log nobody reads,
// which is the same as no log at all.
//
// Levels fix the volume. A JSON mode fixes the other half: a line like
// `accepted block 41 0000abc…` is fine for a human and useless to anything that
// wants to count blocks per hour, so the same events can be emitted as objects
// with the fields already separated.
//
// This wraps the standard logger rather than replacing it, so any code still
// calling log.Printf keeps working and lands at info level.

// Level is a logging verbosity.
type Level int32

const (
	LevelError Level = iota // only things that are going wrong
	LevelWarn               // and things that probably are
	LevelInfo               // the default: blocks, peers, mining
	LevelDebug              // per-message detail
)

// levelNames maps levels to their operator-facing names, lowest first.
var levelNames = []string{"error", "warn", "info", "debug"}

func (l Level) String() string {
	if l < 0 || int(l) >= len(levelNames) {
		return "info"
	}
	return levelNames[l]
}

// ParseLevel resolves a level name. It is strict: a misspelled level would
// otherwise silently select a verbosity the operator did not ask for.
func ParseLevel(name string) (Level, error) {
	for i, n := range levelNames {
		if strings.EqualFold(name, n) {
			return Level(i), nil
		}
	}
	return LevelInfo, fmt.Errorf("unknown log level %q (want one of %s)", name, strings.Join(levelNames, ", "))
}

var (
	logLevel atomic.Int32 // current verbosity
	logJSON  atomic.Bool  // emit objects instead of prose
)

func init() { logLevel.Store(int32(LevelInfo)) }

// SetLogLevel sets the process-wide verbosity.
func SetLogLevel(l Level) { logLevel.Store(int32(l)) }

// LogLevel returns the current verbosity.
func LogLevel() Level { return Level(logLevel.Load()) }

// SetLogJSON switches output between prose and one JSON object per line.
func SetLogJSON(on bool) {
	logJSON.Store(on)
	if on {
		// The standard logger's timestamp prefix would corrupt each JSON line, and
		// the object carries its own.
		log.SetFlags(0)
	}
}

// enabled reports whether a level would be printed.
func enabled(l Level) bool { return int32(l) <= logLevel.Load() }

// Logf emits a message at a level. Fields are optional key/value pairs, used as
// the object's fields in JSON mode and appended as `key=value` otherwise — so
// the same call site is readable either way.
func Logf(l Level, event string, fields ...any) {
	if !enabled(l) {
		return
	}
	if !logJSON.Load() {
		if len(fields) == 0 {
			log.Print(event)
			return
		}
		log.Printf("%s %s", event, pairs(fields))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, `{"level":%q,"event":%q`, l.String(), event)
	for i := 0; i+1 < len(fields); i += 2 {
		fmt.Fprintf(&b, `,%q:`, fmt.Sprint(fields[i]))
		switch v := fields[i+1].(type) {
		case string:
			fmt.Fprintf(&b, "%q", v)
		case error:
			fmt.Fprintf(&b, "%q", v.Error())
		case bool, int, int64, uint64, float64:
			fmt.Fprintf(&b, "%v", v)
		default:
			fmt.Fprintf(&b, "%q", fmt.Sprint(v))
		}
	}
	b.WriteString("}")
	log.Print(b.String())
}

// pairs renders key/value fields for the prose form.
func pairs(fields []any) string {
	var parts []string
	for i := 0; i+1 < len(fields); i += 2 {
		parts = append(parts, fmt.Sprintf("%v=%v", fields[i], fields[i+1]))
	}
	return strings.Join(parts, " ")
}

// Convenience wrappers, so call sites read as the thing they are reporting.
func Errorf(event string, fields ...any) { Logf(LevelError, event, fields...) }
func Warnf(event string, fields ...any)  { Logf(LevelWarn, event, fields...) }
func Infof(event string, fields ...any)  { Logf(LevelInfo, event, fields...) }
func Debugf(event string, fields ...any) { Logf(LevelDebug, event, fields...) }

// LogTo redirects log output, for a node that logs somewhere other than stderr.
func LogTo(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	log.SetOutput(f)
	return func() {
		log.SetOutput(os.Stderr)
		_ = f.Close()
	}, nil
}
