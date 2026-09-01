package whitelist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	notchUUID = "069a79f4-44e9-4726-a5be-fca90e38aaf5"
	alexUUID  = "853c80ef-3c37-49fd-aa49-938b674adae6"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "whitelist.txt")
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func open(t *testing.T, body string) (*List, string) {
	t.Helper()
	p := write(t, body)
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return l, p
}

func TestCheck(t *testing.T) {
	l, _ := open(t, "# friends\nNotch:"+notchUUID+"\n\nAlex:"+alexUUID+"\n")

	for _, tc := range []struct {
		name, ign, uuid string
		want            bool
	}{
		{"uuid match", "Notch", notchUUID, true},
		{"uuid match, bare hex", "Notch", strings.ReplaceAll(notchUUID, "-", ""), true},
		{"uuid match, upper case", "Notch", strings.ToUpper(notchUUID), true},
		{"uuid not listed", "Notch", "11111111-2222-3333-4444-555555555555", false},
		// Pre-1.19 clients send no UUID at all, so the IGN is all there is.
		{"ign fallback", "Alex", "", true},
		{"ign fallback, wrong case", "aLeX", "", true},
		{"ign fallback, not listed", "Herobrine", "", false},
		// An unlisted UUID is refused even when the IGN is on the list: the UUID is
		// the stronger claim of the two, so it decides.
		{"listed ign, unlisted uuid", "Alex", "11111111-2222-3333-4444-555555555555", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := l.Check(tc.ign, tc.uuid); got != tc.want {
				t.Fatalf("Check(%q, %q) = %v, want %v", tc.ign, tc.uuid, got, tc.want)
			}
		})
	}
}

func TestEmptyListDeniesEveryone(t *testing.T) {
	l, _ := open(t, "# nobody yet\n")
	if l.Check("Notch", notchUUID) || l.Check("Notch", "") {
		t.Fatal("empty list let someone in")
	}
	if l.Len() != 0 {
		t.Fatalf("Len = %d", l.Len())
	}
}

// A player who renames keeps their UUID. The stored IGN is updated so the pre-1.19
// fallback keeps working for them.
func TestRenameRewritesFile(t *testing.T) {
	l, p := open(t, "# friends\nNotch:"+notchUUID+"\nAlex:"+alexUUID+"\n")

	if !l.Check("Notch2", notchUUID) {
		t.Fatal("rename should still match on uuid")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := "# friends\nNotch2:" + notchUUID + "\nAlex:" + alexUUID + "\n"
	if string(b) != want {
		t.Fatalf("file is\n%q\nwant\n%q", b, want)
	}
	// The new name works on a client too old to send a UUID; the old one does not.
	if !l.Check("Notch2", "") {
		t.Error("renamed player rejected by ign fallback")
	}
	if l.Check("Notch", "") {
		t.Error("stale ign still accepted")
	}
}

func TestRenameIsNotWrittenWhenNameMatches(t *testing.T) {
	l, p := open(t, "Notch:"+notchUUID+"\n")
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	// Case differences are not renames.
	if !l.Check("nOtCh", notchUUID) {
		t.Fatal("case-different ign rejected")
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("file rewritten for a name that already matched")
	}
}

// The list can be edited on the node without restarting proxyd, which would drop
// every player mid-session.
func TestReloadPicksUpEdits(t *testing.T) {
	l, p := open(t, "Notch:"+notchUUID+"\n")
	if l.Check("Alex", alexUUID) {
		t.Fatal("Alex was not on the list yet")
	}
	if err := os.WriteFile(p, []byte("Notch:"+notchUUID+"\nAlex:"+alexUUID+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if !l.Check("Alex", alexUUID) {
		t.Fatal("edit not picked up")
	}
}

// A half-written file must not open or close the gate. The last good list stands.
func TestBrokenReloadKeepsLastGoodList(t *testing.T) {
	l, p := open(t, "Notch:"+notchUUID+"\n")
	if err := os.WriteFile(p, []byte("Notch:not-a-uuid\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if !l.Check("Notch", notchUUID) {
		t.Fatal("a broken edit locked out a listed player")
	}
	if l.Check("Herobrine", "") {
		t.Fatal("a broken edit let a stranger in")
	}
}

func TestOpenRejectsBadInput(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no uuid column", "Notch\n"},
		{"uuid too short", "Notch:069a79f4\n"},
		{"uuid not hex", "Notch:zzzzzzzz-44e9-4726-a5be-fca90e38aaf5\n"},
		{"empty ign", ":" + notchUUID + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Open(write(t, tc.body)); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

// An ingress told to be gated must not come up ungated.
func TestOpenMissingFileFails(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "absent.txt")); err == nil {
		t.Fatal("missing whitelist accepted")
	}
}
