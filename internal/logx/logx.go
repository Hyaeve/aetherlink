// Package logx provides leveled logging with an in-memory ring buffer so the
// admin UI can display recent server logs without touching the filesystem.
package logx

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "info"
	}
}

func ParseLevel(name string) Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug", "trace":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error", "fatal":
		return LevelError
	default:
		return LevelInfo
	}
}

// Entry is a single buffered log record exposed through the admin API.
type Entry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

type ringBuffer struct {
	mu      sync.RWMutex
	entries []Entry
	max     int
}

var (
	logger  = log.New(os.Stderr, "", 0)
	buffer  = &ringBuffer{max: 5000}
	minimum = LevelInfo
	levelMu sync.RWMutex
)

// SetLevel changes the minimum level that is emitted and buffered.
func SetLevel(l Level) {
	levelMu.Lock()
	minimum = l
	levelMu.Unlock()
}

// SetMaxEntries resizes the ring buffer used by the admin API.
func SetMaxEntries(max int) {
	if max < 50 {
		max = 50
	}
	buffer.mu.Lock()
	buffer.max = max
	if len(buffer.entries) > max {
		buffer.entries = append([]Entry(nil), buffer.entries[len(buffer.entries)-max:]...)
	}
	buffer.mu.Unlock()
}

func enabled(l Level) bool {
	levelMu.RLock()
	defer levelMu.RUnlock()
	return l >= minimum
}

func emit(l Level, format string, args ...any) {
	if !enabled(l) {
		return
	}
	entry := Entry{Time: time.Now(), Level: l.String(), Message: fmt.Sprintf(format, args...)}
	buffer.mu.Lock()
	buffer.entries = append(buffer.entries, entry)
	if len(buffer.entries) > buffer.max {
		buffer.entries = append([]Entry(nil), buffer.entries[len(buffer.entries)-buffer.max:]...)
	}
	buffer.mu.Unlock()
	logger.Printf("%s %-5s %s", entry.Time.Format("2006-01-02 15:04:05.000"), strings.ToUpper(entry.Level), entry.Message)
}

func Debugf(format string, args ...any) { emit(LevelDebug, format, args...) }
func Infof(format string, args ...any)  { emit(LevelInfo, format, args...) }
func Warnf(format string, args ...any)  { emit(LevelWarn, format, args...) }
func Errorf(format string, args ...any) { emit(LevelError, format, args...) }

// Recent returns up to limit buffered entries, newest last.
func Recent(limit int) []Entry {
	buffer.mu.RLock()
	defer buffer.mu.RUnlock()
	if limit <= 0 || limit > len(buffer.entries) {
		limit = len(buffer.entries)
	}
	out := make([]Entry, limit)
	copy(out, buffer.entries[len(buffer.entries)-limit:])
	return out
}
