package jsonl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type rec struct {
	N    int    `json:"n"`
	Name string `json:"name"`
}

func write(t *testing.T, w *Writer, n int, name string) {
	t.Helper()
	if err := w.Write(rec{N: n, Name: name}); err != nil {
		t.Fatal(err)
	}
}

func TestTailReturnsTheNewest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	w, err := NewWriter(p, 8)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		write(t, w, i, "a")
	}
	w.Close()

	got, err := Tail[rec](p, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].N != 8 || got[2].N != 10 {
		t.Fatalf("tail = %+v, want the last three oldest-first", got)
	}
}

func TestTailFilters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	w, _ := NewWriter(p, 8)
	for i := 1; i <= 6; i++ {
		name := "a"
		if i%2 == 0 {
			name = "b"
		}
		write(t, w, i, name)
	}
	w.Close()

	got, _ := Tail[rec](p, 10, func(r *rec) bool { return r.Name == "b" })
	if len(got) != 3 {
		t.Fatalf("filtered tail = %+v, want three", got)
	}
}

// A rotated file holds what came before the current one, so the newest records
// can span both.
func TestTailReadsAcrossARotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	w, _ := NewWriter(p, 0)
	write(t, w, 1, "old")
	w.Close()
	if err := os.Rename(p, p+".1"); err != nil {
		t.Fatal(err)
	}
	w, _ = NewWriter(p, 0)
	write(t, w, 2, "new")
	w.Close()

	got, _ := Tail[rec](p, 10, nil)
	if len(got) != 2 || got[0].N != 1 || got[1].N != 2 {
		t.Fatalf("tail across rotation = %+v, want the old one first", got)
	}
}

// A crash mid-write leaves a truncated last line. The records before it are
// still good and losing all of them would be the worse answer.
func TestTailSkipsATruncatedLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(p, []byte(`{"n":1}`+"\n"+`{"n":2,"na`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Tail[rec](p, 10, nil)
	if err != nil {
		t.Fatalf("a truncated line failed the whole read: %v", err)
	}
	if len(got) != 1 || got[0].N != 1 {
		t.Fatalf("tail = %+v, want just the intact record", got)
	}
}

func TestTailOnAFileThatIsNotThere(t *testing.T) {
	got, err := Tail[rec](filepath.Join(t.TempDir(), "nothing.jsonl"), 10, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("tail = %+v, %v; want nothing and no error", got, err)
	}
}

// The probe dataset spells a leg "hk>ty", and encoding/json would spell it
// "hk>ty" by default. These are files people grep.
func TestWriteDoesNotEscapeForHTML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	w, _ := NewWriter(p, 8)
	write(t, w, 1, "hk>ty")
	w.Close()

	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "hk>ty") {
		t.Fatalf("record was escaped: %s", b)
	}
}

func TestRotationKeepsOnePreviousFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	w, err := NewWriter(p, 1) // 1 MB
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 4096)
	for i := 0; i < 400; i++ {
		write(t, w, i, big)
	}
	w.Close()

	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > 1<<20 {
		t.Errorf("current file is %d bytes, past its bound", st.Size())
	}
}
