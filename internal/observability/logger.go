// Package observability provides structured logging and request-scoped tracing helpers.
package observability

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// NewLogger returns a JSON slog logger. level accepts debug/info/warn/error.
func NewLogger(level string, w io.Writer) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}))
}

// OpenDailyLogFile returns a writer that appends to path and rotates it at
// local midnight into path-YYYY-MM-DD.<ext>. Rotated files older than
// maxAgeDays are pruned; zero keeps them forever. The writer is safe for
// concurrent use.
func OpenDailyLogFile(path string, maxAgeDays int) (io.WriteCloser, error) {
	if path == "" {
		return nil, fmt.Errorf("log path must not be empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	r := &rotateWriter{
		path:       path,
		dir:        filepath.Dir(path),
		base:       strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)),
		ext:        filepath.Ext(path),
		maxAgeDays: maxAgeDays,
	}
	if err := r.reopen(time.Now()); err != nil {
		return nil, err
	}
	return r, nil
}

type rotateWriter struct {
	path       string
	dir        string
	base       string
	ext        string
	maxAgeDays int

	mu      sync.Mutex
	file    *os.File
	curDate string // YYYY-MM-DD of the currently open file
}

func (r *rotateWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	today := time.Now().Format("2006-01-02")
	if today != r.curDate {
		if err := r.rotateLocked(today); err != nil {
			return 0, err
		}
	}
	return r.file.Write(p)
}

func (r *rotateWriter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		err := r.file.Close()
		r.file = nil
		return err
	}
	return nil
}

// reopen opens (creating if needed) the active log file for the given day.
func (r *rotateWriter) reopen(now time.Time) error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", r.path, err)
	}
	r.file = f
	r.curDate = now.Format("2006-01-02")
	return nil
}

// rotateLocked closes the current file, renames it to the dated backup, opens
// a fresh file and prunes old backups. The caller must hold r.mu.
func (r *rotateWriter) rotateLocked(today string) error {
	if r.file != nil {
		_ = r.file.Close()
	}
	// The current file holds content from the previous day(s); move it aside.
	backup := filepath.Join(r.dir, r.base+"-"+r.curDate+r.ext)
	if _, err := os.Stat(r.path); err == nil {
		if _, statErr := os.Stat(backup); statErr != nil {
			_ = os.Rename(r.path, backup)
		}
	}
	if err := r.reopen(time.Now()); err != nil {
		return err
	}
	r.pruneLocked()
	return nil
}

// pruneLocked removes dated backups older than maxAgeDays.
func (r *rotateWriter) pruneLocked() {
	if r.maxAgeDays <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -r.maxAgeDays)
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	type backup struct {
		path string
		date string
	}
	var backups []backup
	prefix := r.base + "-"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, r.ext) {
			continue
		}
		datePart := strings.TrimSuffix(strings.TrimPrefix(name, prefix), r.ext)
		if _, err := time.Parse("2006-01-02", datePart); err != nil {
			continue
		}
		backups = append(backups, backup{filepath.Join(r.dir, name), datePart})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].date < backups[j].date })
	for _, b := range backups {
		d, err := time.Parse("2006-01-02", b.date)
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			_ = os.Remove(b.path)
		}
	}
}
