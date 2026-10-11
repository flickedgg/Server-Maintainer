package core

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RotatingFile is an io.Writer that rolls path over to path.1, path.2, ... once it
// grows past maxSize, keeping at most maxFiles old files.
type RotatingFile struct {
	mu       sync.Mutex
	path     string
	maxSize  int64
	maxFiles int
	f        *os.File
	size     int64
}

func OpenRotating(path string, maxSize int64, maxFiles int) (*RotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	r := &RotatingFile{path: path, maxSize: maxSize, maxFiles: maxFiles}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, info.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil && r.size > 0 && r.size+int64(len(p)) > r.maxSize {
		r.rotate()
	}
	if r.f == nil {
		return 0, os.ErrClosed
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) rotate() {
	r.f.Close()
	r.f = nil
	backup := func(i int) string { return fmt.Sprintf("%s.%d", r.path, i) }
	os.Remove(backup(r.maxFiles))
	for i := r.maxFiles - 1; i >= 1; i-- {
		os.Rename(backup(i), backup(i+1))
	}
	if err := os.Rename(r.path, backup(1)); err != nil {
		fmt.Fprintf(os.Stderr, "log rotation of %s failed: %v\n", r.path, err)
	}
	if err := r.open(); err != nil {
		fmt.Fprintf(os.Stderr, "reopening %s failed: %v\n", r.path, err)
	}
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// consoleTee writes to the log file and to stdout, ignoring stdout errors so a
// service without a console still gets its log file.
type consoleTee struct{ file io.Writer }

func (t consoleTee) Write(p []byte) (int, error) {
	n, err := t.file.Write(p)
	os.Stdout.Write(p)
	return n, err
}

// SetupLogging returns the maintainer logger and the writer for server console output.
func SetupLogging(cfg LogConfig) (*slog.Logger, *RotatingFile, func(), error) {
	maxSize := int64(cfg.MaxSizeMB) << 20
	mainLog, err := OpenRotating(filepath.Join(cfg.Dir, "maintainer.log"), maxSize, cfg.MaxFiles)
	if err != nil {
		return nil, nil, nil, err
	}
	serverLog, err := OpenRotating(filepath.Join(cfg.Dir, "server.log"), maxSize, cfg.MaxFiles)
	if err != nil {
		mainLog.Close()
		return nil, nil, nil, err
	}
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Level)) // validated in Config.Validate
	logger := slog.New(slog.NewTextHandler(consoleTee{mainLog}, &slog.HandlerOptions{Level: level}))
	closeAll := func() {
		mainLog.Close()
		serverLog.Close()
	}
	return logger, serverLog, closeAll, nil
}

// WriteConsoleLine appends one timestamped server console line to w.
func WriteConsoleLine(w io.Writer, line string) {
	buf := make([]byte, 0, len(line)+21)
	buf = time.Now().AppendFormat(buf, "2006-01-02 15:04:05 ")
	buf = append(buf, line...)
	buf = append(buf, '\n')
	w.Write(buf)
}

// IsConsoleNoise matches console lines that carry no information. CS2 on Windows
// prints "CTextConsoleWin::GetLine: !GetNumberOfConsoleInputEvents" in a loop
// when its input isn't a real console (server.console_window is false).
func IsConsoleNoise(line string) bool {
	return strings.Contains(line, "CTextConsoleWin::GetLine")
}

// EchoServerLine prints one server console line to w with a [cs2] prefix, in a
// single write so it doesn't interleave with the maintainer's own log lines.
func EchoServerLine(w io.Writer, line string) {
	buf := make([]byte, 0, len(line)+7)
	buf = append(buf, "[cs2] "...)
	buf = append(buf, line...)
	buf = append(buf, '\n')
	w.Write(buf)
}

// LineWriter splits a byte stream into lines and calls fn for each one.
// It is not safe for concurrent use; os/exec serializes writes when Stdout and
// Stderr are the same writer.
type LineWriter struct {
	buf []byte
	fn  func(string)
}

const maxLineLen = 64 << 10

func NewLineWriter(fn func(string)) *LineWriter { return &LineWriter{fn: fn} }

func (w *LineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.buf = append(w.buf, p...)
			if len(w.buf) >= maxLineLen {
				w.emit()
			}
			break
		}
		w.buf = append(w.buf, p[:i]...)
		w.emit()
		p = p[i+1:]
	}
	return n, nil
}

func (w *LineWriter) emit() {
	line := string(bytes.TrimRight(w.buf, "\r"))
	w.buf = w.buf[:0]
	w.fn(line)
}

// Flush emits any trailing text that had no newline.
func (w *LineWriter) Flush() {
	if len(w.buf) > 0 {
		w.emit()
	}
}

// cappedBuffer collects output up to max bytes and silently drops the rest.
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
