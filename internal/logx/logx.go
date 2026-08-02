// Package logx provides the service's logger. A Windows service has no stderr
// that anyone can see, so logs go to a file under %ProgramData% and, for
// warnings and above, to the Windows event log.
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Level orders the severities we emit.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel maps a config string to a Level, defaulting to info for anything
// unrecognised rather than failing service startup over a typo.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// EventSink receives warn/error messages destined for the Windows event log.
// The service installs one; other entry points leave it nil.
type EventSink interface {
	Warning(eid uint32, msg string) error
	Error(eid uint32, msg string) error
}

// Logger writes leveled messages to an io.Writer and optionally mirrors
// warnings and errors to an EventSink.
type Logger struct {
	mu     sync.Mutex
	out    *log.Logger
	level  Level
	sink   EventSink
	closer io.Closer
}

var defaultLogger = &Logger{
	out:   log.New(io.Discard, "", 0),
	level: LevelInfo,
}

// Default returns the process-wide logger.
func Default() *Logger { return defaultLogger }

// SetDefault replaces the process-wide logger.
func SetDefault(l *Logger) { defaultLogger = l }

// NewWriter builds a Logger over an arbitrary writer. Used by CLI entry points
// that want diagnostics on stderr.
func NewWriter(w io.Writer, level Level) *Logger {
	return &Logger{out: log.New(w, "", log.LstdFlags|log.Lmicroseconds), level: level}
}

// NewFile opens (creating parent directories as needed) the service log file.
// If the file cannot be opened the returned logger discards output rather than
// preventing the service from starting; losing logs is better than losing the
// service.
func NewFile(path string, level Level) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return NewWriter(io.Discard, level), err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return NewWriter(io.Discard, level), err
	}
	return &Logger{
		out:    log.New(f, "", log.LstdFlags|log.Lmicroseconds),
		level:  level,
		closer: f,
	}, nil
}

// SetSink attaches an event-log sink for warn and error messages.
func (l *Logger) SetSink(s EventSink) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sink = s
}

// Close releases the underlying file handle, if any.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closer != nil {
		return l.closer.Close()
	}
	return nil
}

func (l *Logger) logf(lv Level, format string, args ...any) {
	if l == nil || lv < l.level {
		return
	}
	msg := fmt.Sprintf(format, args...)
	l.mu.Lock()
	l.out.Printf("[%s] %s", lv, msg)
	sink := l.sink
	l.mu.Unlock()

	if sink == nil {
		return
	}
	// Event log IDs: 1 for warnings, 2 for errors. Keeping them fixed means the
	// event log can be filtered without tracking a growing ID table.
	switch lv {
	case LevelWarn:
		_ = sink.Warning(1, msg)
	case LevelError:
		_ = sink.Error(2, msg)
	}
}

func (l *Logger) Debugf(format string, args ...any) { l.logf(LevelDebug, format, args...) }
func (l *Logger) Infof(format string, args ...any)  { l.logf(LevelInfo, format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.logf(LevelWarn, format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.logf(LevelError, format, args...) }

// Package-level shortcuts operating on the default logger.
func Debugf(format string, args ...any) { defaultLogger.Debugf(format, args...) }
func Infof(format string, args ...any)  { defaultLogger.Infof(format, args...) }
func Warnf(format string, args ...any)  { defaultLogger.Warnf(format, args...) }
func Errorf(format string, args ...any) { defaultLogger.Errorf(format, args...) }
