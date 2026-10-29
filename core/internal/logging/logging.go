// Package logging writes a size-rotated daemon log.
package logging

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger is a small rotating logger.
type Logger struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	size     int64
	maxBytes int64
	maxFiles int
}

// New creates the log file inside dir.
func New(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "daemon.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	l := &Logger{path: path, file: f, maxBytes: 5 << 20, maxFiles: 3}
	if st != nil {
		l.size = st.Size()
	}
	return l, nil
}

// Path returns the log file path.
func (l *Logger) Path() string { return l.path }

// Printf writes one line.
func (l *Logger) Printf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	line := fmt.Sprintf("%s %-5s %s\n", time.Now().Format(time.RFC3339), level, msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(line)) > l.maxBytes {
		l.rotateLocked()
	}
	n, _ := l.file.WriteString(line)
	l.size += int64(n)
}

func (l *Logger) rotateLocked() {
	l.file.Close()
	os.Remove(fmt.Sprintf("%s.%d", l.path, l.maxFiles))
	for i := l.maxFiles - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	os.Rename(l.path, l.path+".1")
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err == nil {
		l.file = f
		l.size = 0
	}
}

// Close closes the log file.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
	}
}

// Tail returns the last n lines of the current log file.
func (l *Logger) Tail(n int) ([]string, error) {
	if n <= 0 || n > 5000 {
		n = 200
	}
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	ring := make([]string, 0, n)
	for sc.Scan() {
		if len(ring) == n {
			copy(ring, ring[1:])
			ring[n-1] = sc.Text()
		} else {
			ring = append(ring, sc.Text())
		}
	}
	return ring, sc.Err()
}
