package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteAppendsDailyJSONLines(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 9, 12, 23, 59, 0, 0, time.UTC)
	l.now = func() time.Time { return day }
	for i := 0; i < 2; i++ {
		if err := l.Write(map[string]any{"n": i, "url": "a?b=1&c=2"}); err != nil {
			t.Fatal(err)
		}
	}
	day = day.Add(2 * time.Minute)
	if err := l.Write(map[string]any{"n": 2}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	count := func(name string) int {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		n := 0
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var v map[string]any
			if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
				t.Fatalf("line is not JSON: %s", sc.Text())
			}
			n++
		}
		return n
	}
	if count("2026-09-12.jsonl") != 2 || count("2026-09-13.jsonl") != 1 {
		t.Fatal("unexpected line counts")
	}
}

func TestDisabledLoggerAndBody(t *testing.T) {
	var nilLogger *Logger
	if err := nilLogger.Write(map[string]any{}); err != nil {
		t.Fatal(err)
	}
	l, _ := New("")
	if err := l.Write(map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := string(Body([]byte(`{ "a" : 1 }`))); got != `{"a":1}` {
		t.Fatalf("Body compacts JSON, got %s", got)
	}
	if got := string(Body([]byte("not json"))); got != `"not json"` {
		t.Fatalf("Body quotes non-JSON, got %s", got)
	}
	if Body(nil) != nil {
		t.Fatal("empty body should be nil")
	}
}
