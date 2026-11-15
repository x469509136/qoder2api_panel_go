// Package logx is the gateway's logging and in-memory log ring.
//
// It has three sinks: a rotating file, stderr, and a bounded in-memory ring
// that /logs reads. Each entry carries a level and a tag so the panel can
// filter by subsystem without grepping the file.
package logx

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Levels are rendered as fix-width tags in the log line.
const (
	LevelDebug = "DEBUG"
	LevelInfo  = "INFO"
	LevelWarn  = "WARN"
	LevelError = "ERROR"
)

// Entry is one log line as /logs returns it.
type Entry struct {
	ID    int64  `json:"id"`
	At    int64  `json:"at"`
	ISO   string `json:"iso"`
	Level string `json:"level"`
	Tag   string `json:"tag"`
	Msg   string `json:"msg"`
}

// Logger fans writes out to a file, stderr and a bounded ring.
type Logger struct {
	mu       sync.Mutex
	ring     []Entry
	ringCap  int
	nextID   int64
	file     *os.File
	filePath string
	maxBytes int64
	echo     io.Writer
}

// Options configures a Logger.
type Options struct {
	// Dir is where gateway.log is written; empty disables file output.
	Dir string
	// FileName is the log file name (default "gateway.log").
	FileName string
	// RingSize is how many entries /logs can return (default 2000).
	RingSize int
	// MaxBytes triggers a single .1 rotation (default 16 MiB).
	MaxBytes int64
	// Echo receives a copy of every line; nil means stderr.
	Echo io.Writer
}

// New creates a Logger.
func New(opts Options) *Logger {
	if opts.RingSize <= 0 {
		opts.RingSize = 2000
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 16 << 20
	}
	if opts.FileName == "" {
		opts.FileName = "gateway.log"
	}
	l := &Logger{
		ring:     make([]Entry, 0, opts.RingSize),
		ringCap:  opts.RingSize,
		maxBytes: opts.MaxBytes,
		echo:     opts.Echo,
	}
	if l.echo == nil {
		l.echo = os.Stderr
	}
	if opts.Dir != "" {
		if err := os.MkdirAll(opts.Dir, 0o755); err == nil {
			path := filepath.Join(opts.Dir, opts.FileName)
			l.filePath = path
			l.rotateIfNeeded()
			if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				l.file = f
			}
		}
	}
	return l
}

// Close flushes and closes the log file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

// FilePath returns the current log file path ("" when file logging is off).
func (l *Logger) FilePath() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.filePath
}

func (l *Logger) rotateIfNeeded() {
	if l.filePath == "" {
		return
	}
	if fi, err := os.Stat(l.filePath); err == nil && fi.Size() > l.maxBytes {
		_ = os.Rename(l.filePath, l.filePath+".1")
	}
}

// Log writes one message.
func (l *Logger) Log(level, tag, msg string) {
	if level == "" {
		level = LevelInfo
	}
	now := time.Now()
	line := fmt.Sprintf("[%s] %-5s %-12s %s\n",
		now.Format("2006-01-02 15:04:05.000"), level, tag, msg)

	l.mu.Lock()
	entry := Entry{
		ID:    l.nextID,
		At:    now.Unix(),
		ISO:   now.Format("2006-01-02T15:04:05"),
		Level: level,
		Tag:   tag,
		Msg:   msg,
	}
	l.nextID++
	l.ring = append(l.ring, entry)
	if len(l.ring) > l.ringCap {
		drop := len(l.ring) - l.ringCap
		l.ring = append(l.ring[:0], l.ring[drop:]...)
	}
	if l.file != nil {
		_, _ = l.file.WriteString(line)
	}
	echo := l.echo
	l.mu.Unlock()

	if echo != nil {
		_, _ = io.WriteString(echo, line)
	}
}

// Info logs at INFO.
func (l *Logger) Info(tag, format string, args ...any) {
	l.Log(LevelInfo, tag, fmt.Sprintf(format, args...))
}

// Warn logs at WARN.
func (l *Logger) Warn(tag, format string, args ...any) {
	l.Log(LevelWarn, tag, fmt.Sprintf(format, args...))
}

// Error logs at ERROR.
func (l *Logger) Error(tag, format string, args ...any) {
	l.Log(LevelError, tag, fmt.Sprintf(format, args...))
}

// Query describes a /logs request.
type Query struct {
	Limit   int
	Level   string
	Tag     string
	Search  string
	SinceID int64
}

// Query returns the most recent matching entries, oldest first.
func (l *Logger) Query(q Query) []Entry {
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 200
	}
	level := strings.ToUpper(strings.TrimSpace(q.Level))
	search := strings.ToLower(strings.TrimSpace(q.Search))
	tag := strings.TrimSpace(q.Tag)

	l.mu.Lock()
	matches := make([]Entry, 0, q.Limit)
	for i := len(l.ring) - 1; i >= 0 && len(matches) < q.Limit; i-- {
		e := l.ring[i]
		if q.SinceID > 0 && e.ID <= q.SinceID {
			break
		}
		if level != "" && e.Level != level {
			continue
		}
		if tag != "" && e.Tag != tag {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(e.Msg), search) {
			continue
		}
		matches = append(matches, e)
	}
	l.mu.Unlock()

	// Reverse into chronological order.
	for i, j := 0, len(matches)-1; i < j; i, j = i+1, j-1 {
		matches[i], matches[j] = matches[j], matches[i]
	}
	return matches
}

// Clear empties the in-memory ring (the file is left alone).
func (l *Logger) Clear() {
	l.mu.Lock()
	l.ring = l.ring[:0]
	l.mu.Unlock()
}

// Tags returns the distinct tags currently in the ring, sorted.
func (l *Logger) Tags() []string {
	l.mu.Lock()
	set := map[string]bool{}
	for _, e := range l.ring {
		if e.Tag != "" {
			set[e.Tag] = true
		}
	}
	l.mu.Unlock()
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Discard returns a logger that writes nowhere (useful in tests).
func Discard() *Logger {
	return &Logger{ring: make([]Entry, 0, 8), ringCap: 8, maxBytes: 1 << 20, echo: io.Discard}
}
