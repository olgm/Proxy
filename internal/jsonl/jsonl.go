// Package jsonl appends records to a newline-delimited JSON file and keeps it
// bounded, and reads the newest of them back.
//
// It is shared by every service that needs to write down finished sessions or
// samples the same way. Both files are read by people with grep as often as by
// anything here, which is what the format is for.
package jsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Writer appends records and keeps the file bounded. One previous file is kept,
// so the whole thing never occupies more than twice the configured size however
// long a node runs.
type Writer struct {
	mu     sync.Mutex
	path   string
	maxLen int64
	f      *os.File
	n      int64
}

// NewWriter opens the file, creating its directory. maxMB of zero means no
// bound, which is only correct for a file something else is rotating.
func NewWriter(path string, maxMB int) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	w := &Writer{path: path, maxLen: int64(maxMB) << 20}
	return w, w.open()
}

func (w *Writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.n = f, st.Size()
	return nil
}

// Write appends one record.
func (w *Writer) Write(v any) error {
	// encoding/json escapes > and < for HTML by default, which would spell every
	// leg in the probe dataset "hk>ty". These are files people grep.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	b := buf.Bytes() // Encode already ends the line

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.maxLen > 0 && w.n+int64(len(b)) > w.maxLen {
		if err := w.rotate(); err != nil {
			return err
		}
	}
	n, err := w.f.Write(b)
	w.n += int64(n)
	return err
}

func (w *Writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return err
	}
	return w.open()
}

func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

// Tail returns the newest records that keep says to keep, oldest first, across
// the current file and the one previous one.
//
// It reads forward and holds only what it is keeping, so the cost is the size of
// the file in time and the size of the answer in memory. That is the right way
// round for a file bounded at tens of megabytes and read by a human a few times
// a day, and it avoids a reverse scan that would have to understand where a line
// begins.
func Tail[T any](path string, limit int, keep func(*T) bool) ([]T, error) {
	if limit <= 0 {
		return nil, nil
	}
	ring := make([]T, 0, limit)
	// The rotated file first: it holds what came before the current one.
	for _, p := range []string{path + ".1", path} {
		if err := scan(p, limit, keep, &ring); err != nil {
			return nil, err
		}
	}
	return ring, nil
}

func scan[T any](path string, limit int, keep func(*T) bool, ring *[]T) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil // no rotated file yet, or nothing written at all
	}
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var v T
		// A truncated last line is what a crash mid-write leaves behind. Skip
		// it rather than failing the whole read: the other records are fine.
		if json.Unmarshal(line, &v) != nil {
			continue
		}
		if keep != nil && !keep(&v) {
			continue
		}
		if len(*ring) == limit {
			copy(*ring, (*ring)[1:])
			*ring = (*ring)[:limit-1]
		}
		*ring = append(*ring, v)
	}
	return sc.Err()
}
