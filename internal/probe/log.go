package probe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// line is one record of the dataset. One object per class per window, newline
// delimited, so it can be tailed while it is being written and cut with anything
// that reads a line at a time. The Discord bot will graph it; nothing about the
// format assumes that.
type line struct {
	T     string  `json:"t"`
	W     string  `json:"w"`
	Class string  `json:"class"`
	Kind  string  `json:"kind"`
	Dup   int     `json:"dup"`
	N     int     `json:"n"`
	Sent  int     `json:"sent"`
	Got   int     `json:"got"`
	Fwd   *int    `json:"fwd"`
	Loss  float64 `json:"loss"`
	// LossFwd and LossRev split the round trip into the direction that dropped.
	// Null rather than zero when the responder's counters could not be read across
	// this window: no answers arrived, so nothing is known about either direction
	// beyond the fact that the round trip failed.
	LossFwd *float64 `json:"loss_fwd"`
	LossRev *float64 `json:"loss_rev"`

	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P90  float64 `json:"p90"`
	P99  float64 `json:"p99"`
	Mdev float64 `json:"mdev"`
}

// writer appends reports to the dataset and keeps it bounded. One previous file
// is kept, so the whole thing never occupies more than twice the configured size
// however long a node runs.
type writer struct {
	mu     sync.Mutex
	path   string
	maxLen int64
	f      *os.File
	n      int64
}

func newWriter(path string, maxMB int) (*writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	w := &writer{path: path, maxLen: int64(maxMB) << 20}
	return w, w.open()
}

func (w *writer) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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

func (w *writer) write(r *Report) error {
	// encoding/json escapes > and < for HTML by default, which would spell every
	// leg in the dataset "hk\u003ety". This is a file people grep.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(record(r)); err != nil {
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

func (w *writer) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return err
	}
	return w.open()
}

func (w *writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

func record(r *Report) line {
	fwd, rev, rt := r.Loss()
	l := line{
		T: r.At.Format(time.RFC3339), W: dur(r.Window),
		Class: r.Class, Kind: r.Kind, Dup: r.Dup,
		N: r.N, Sent: r.Sent, Got: r.Got, Loss: round(rt),
		Min: round(r.Min), Max: round(r.Max), Mean: round(r.Mean),
		P50: round(r.P50), P90: round(r.P90), P99: round(r.P99), Mdev: round(r.Mdev),
	}
	if r.Fwd >= 0 {
		n := r.Fwd
		l.Fwd = &n
	}
	if fwd >= 0 {
		v := round(fwd)
		l.LossFwd = &v
	}
	if rev >= 0 {
		v := round(rev)
		l.LossRev = &v
	}
	return l
}

// round keeps a line short. Three decimals is well past the resolution of a
// measurement made across a continent.
func round(v float64) float64 { return math.Round(v*1000) / 1000 }

// dur prints a window the way it was configured rather than the way Go would:
// "10m", not "10m0s".
func dur(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}
