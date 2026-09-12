// Package audit appends one JSON line per record to a daily file.
package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger writes records to <dir>/YYYY-MM-DD.jsonl (UTC date). A nil Logger or
// one with an empty directory discards records.
type Logger struct {
	dir string
	now func() time.Time

	mu   sync.Mutex
	day  string
	file *os.File
}

// New creates the directory if needed.
func New(dir string) (*Logger, error) {
	l := &Logger{dir: dir, now: time.Now}
	if dir == "" {
		return l, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("audit dir: %w", err)
	}
	return l, nil
}

// Body returns b as raw JSON when valid, otherwise as a JSON string.
func Body(b []byte) json.RawMessage {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if json.Valid(b) {
		var buf bytes.Buffer
		if err := json.Compact(&buf, b); err == nil {
			return buf.Bytes()
		}
	}
	s, _ := json.Marshal(string(b))
	return s
}

// Write appends record as one JSON line.
func (l *Logger) Write(record any) error {
	if l == nil || l.dir == "" {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(record); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	day := l.now().UTC().Format("2006-01-02")
	if l.file == nil || day != l.day {
		if l.file != nil {
			_ = l.file.Close()
		}
		f, err := os.OpenFile(filepath.Join(l.dir, day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			l.file = nil
			return err
		}
		l.file, l.day = f, day
	}
	_, err := l.file.Write(buf.Bytes())
	return err
}

// Close closes the current file.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
