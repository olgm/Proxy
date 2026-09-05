package whitelist

import (
	"errors"
	"os"
	"testing"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A tag is the only thing the bot has to tell whose line is whose, so a rename
// must not lose it.
func TestTagIsKeptOnRename(t *testing.T) {
	l, p := open(t, "Notch:"+notchUUID+" # discord:42\n")
	if !l.Check("Notch2", notchUUID, testIP) {
		t.Fatal("rename should still match on uuid")
	}
	if got, want := read(t, p), "Notch2:"+notchUUID+" # discord:42\n"; got != want {
		t.Fatalf("file is %q, want %q", got, want)
	}
}

func TestEntries(t *testing.T) {
	l, _ := open(t, "# friends\nNotch:"+notchUUID+" # discord:42\n\nAlex:"+alexUUID+"\n")
	got := l.Entries()
	want := []Entry{
		{Name: "Notch", UUID: notchUUID, Tag: "discord:42"},
		{Name: "Alex", UUID: alexUUID},
	}
	if len(got) != len(want) {
		t.Fatalf("Entries = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestAdd(t *testing.T) {
	l, p := open(t, "# friends\nNotch:"+notchUUID+"\n")
	if err := l.Add("Alex", alexUUID, "discord:7"); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, p), "# friends\nNotch:"+notchUUID+"\nAlex:"+alexUUID+" # discord:7\n"; got != want {
		t.Fatalf("file is %q, want %q", got, want)
	}
	if !l.Check("Alex", alexUUID, testIP) || !l.Check("Alex", "", testIP) {
		t.Fatal("added player not admitted")
	}

	// The UUID is the identity: a second line for it would be a second owner.
	err := l.Add("Alex2", alexUUID, "discord:8")
	var le *ListedError
	if !errors.As(err, &le) || le.Name != "Alex" || le.Tag != "discord:7" {
		t.Fatalf("Add of a listed uuid: %v", err)
	}
	if l.Len() != 2 {
		t.Fatalf("Len = %d after a refused add", l.Len())
	}

	for _, tc := range []struct{ name, uuid string }{
		{"Nobody", "not-a-uuid"},
		{"", notchUUID},
		{"has:colon", "11111111-2222-3333-4444-555555555555"},
		{"has#hash", "11111111-2222-3333-4444-555555555555"},
		{"has space", "11111111-2222-3333-4444-555555555555"},
	} {
		if err := l.Add(tc.name, tc.uuid, ""); err == nil {
			t.Errorf("Add(%q, %q) accepted", tc.name, tc.uuid)
		}
	}
}

func TestAddToEmptyFile(t *testing.T) {
	l, p := open(t, "")
	if err := l.Add("Notch", notchUUID, ""); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, p), "Notch:"+notchUUID+"\n"; got != want {
		t.Fatalf("file is %q, want %q", got, want)
	}
}

func TestRemove(t *testing.T) {
	l, p := open(t, "# friends\nNotch:"+notchUUID+" # discord:42\n# more\nAlex:"+alexUUID+"\n")
	e, err := l.Remove(notchUUID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "Notch" || e.Tag != "discord:42" {
		t.Errorf("removed %+v", e)
	}
	if got, want := read(t, p), "# friends\n# more\nAlex:"+alexUUID+"\n"; got != want {
		t.Fatalf("file is %q, want %q", got, want)
	}
	if l.Check("Notch", notchUUID, testIP) || l.Check("Notch", "", testIP) {
		t.Fatal("removed player still admitted")
	}
	// The line after the removed one moved up; a rename has to land on it.
	if !l.Check("Alex2", alexUUID, testIP) {
		t.Fatal("survivor not admitted")
	}
	if got, want := read(t, p), "# friends\n# more\nAlex2:"+alexUUID+"\n"; got != want {
		t.Fatalf("after rename file is %q, want %q", got, want)
	}

	if _, err := l.Remove(notchUUID); !errors.Is(err, ErrNotListed) {
		t.Fatalf("second Remove: %v", err)
	}
}

// An edit made on the node between two operations must not be undone by the
// second one.
func TestAddKeepsExternalEdit(t *testing.T) {
	l, p := open(t, "Notch:"+notchUUID+"\n")
	if err := os.WriteFile(p, []byte("# edited\nNotch:"+notchUUID+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := l.Add("Alex", alexUUID, ""); err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, p), "# edited\nNotch:"+notchUUID+"\nAlex:"+alexUUID+"\n"; got != want {
		t.Fatalf("file is %q, want %q", got, want)
	}
}

func TestOpenRejectsTaggedGarbage(t *testing.T) {
	if _, err := Open(write(t, "Notch # discord:1\n"), nil); err == nil {
		t.Fatal("a line with a tag but no uuid was accepted")
	}
}
