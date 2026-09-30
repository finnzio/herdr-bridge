package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Level is a log severity.
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
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	default:
		return "error"
	}
}

// ParseLevel maps a flag value to a Level.
func ParseLevel(s string) (Level, error) {
	switch s {
	case "debug":
		return LevelDebug, nil
	case "info", "":
		return LevelInfo, nil
	case "warn":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	}
	return LevelInfo, fmt.Errorf("unknown log level %q", s)
}

// Logger writes JSON lines: {"t":...,"lvl":"info","msg":"...",<fields>}.
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	level Level
}

func NewLogger(w io.Writer, level Level) *Logger {
	return &Logger{w: w, level: level}
}

func (l *Logger) log(lv Level, msg string, kv ...any) {
	if l == nil || lv < l.level {
		return
	}
	rec := make(map[string]any, 4+len(kv)/2)
	rec["t"] = time.Now().Format(time.RFC3339Nano)
	rec["lvl"] = lv.String()
	rec["msg"] = msg
	for i := 0; i+1 < len(kv); i += 2 {
		rec[fmt.Sprint(kv[i])] = kv[i+1]
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(b, '\n'))
}

func (l *Logger) Debug(msg string, kv ...any) { l.log(LevelDebug, msg, kv...) }
func (l *Logger) Info(msg string, kv ...any)  { l.log(LevelInfo, msg, kv...) }
func (l *Logger) Warn(msg string, kv ...any)  { l.log(LevelWarn, msg, kv...) }
func (l *Logger) Error(msg string, kv ...any) { l.log(LevelError, msg, kv...) }
