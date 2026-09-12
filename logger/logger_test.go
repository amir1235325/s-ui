package logger

import (
	"strings"
	"sync"
	"testing"

	"github.com/op/go-logging"
)

// TestBufferConcurrentAccess reproduces the shape of the panel at runtime: cron
// jobs, HTTP handlers and sing-box connection goroutines all log while the
// /logs endpoint reads the buffer. Before addToBuffer and GetLogs shared a
// mutex this tripped the race detector on both the append and the re-slice.
// Run with -race; without it the test only proves nothing panics.
func TestBufferConcurrentAccess(t *testing.T) {
	InitLogger(logging.ERROR)
	resetBuffer()

	const writers = 8
	const perWriter = 400

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < perWriter; j++ {
				Info("writer", n, "line", j)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < perWriter; j++ {
			_ = GetLogs(50, "debug")
		}
	}()
	wg.Wait()

	if got := len(GetLogs(10000, "debug")); got != writers*perWriter {
		t.Errorf("expected every logged line to survive, got %d of %d", got, writers*perWriter)
	}
}

// TestGetLogsRespectsCount locks down an off-by-one: the loop condition was
// `len(output) <= c`, which was still true once c entries had been collected,
// so each call returned c+1 lines.
func TestGetLogsRespectsCount(t *testing.T) {
	InitLogger(logging.ERROR)
	resetBuffer()

	for i := 0; i < 20; i++ {
		Info("line", i)
	}

	for _, count := range []int{0, 1, 5, 20, 50} {
		if got := len(GetLogs(count, "debug")); got > count {
			t.Errorf("GetLogs(%d) returned %d lines, want at most %d", count, got, count)
		}
	}
}

// TestGetLogsFiltersByLevel checks the level filter still excludes finer levels
// than the one requested, so the mutex change did not alter what is returned.
func TestGetLogsFiltersByLevel(t *testing.T) {
	InitLogger(logging.ERROR)
	resetBuffer()

	Debug("a debug line")
	Error("an error line")

	errorsOnly := GetLogs(100, "error")
	if len(errorsOnly) != 1 {
		t.Fatalf("expected only the error line at level error, got %d lines: %v", len(errorsOnly), errorsOnly)
	}
	if !strings.Contains(errorsOnly[0], "an error line") {
		t.Errorf("expected the error line, got %q", errorsOnly[0])
	}

	if got := len(GetLogs(100, "debug")); got != 2 {
		t.Errorf("expected both lines at level debug, got %d", got)
	}
}

// TestBufferIsBounded covers the ring-buffer trim, which is the operation the
// concurrent append used to race against.
func TestBufferIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("fills the 10240-entry buffer")
	}
	InitLogger(logging.ERROR)
	resetBuffer()

	for i := 0; i < 10300; i++ {
		Info("line", i)
	}

	bufferMu.Lock()
	size := len(logBuffer)
	bufferMu.Unlock()
	if size > 10240 {
		t.Errorf("buffer grew past its bound: %d entries", size)
	}
}

func resetBuffer() {
	bufferMu.Lock()
	logBuffer = nil
	bufferMu.Unlock()
}
