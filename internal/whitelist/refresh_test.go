package whitelist

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The attack the refresh exists to stop: a listed player renames away, someone else
// claims the name they released, and logs in on a client too old to send a UUID.
func TestRefreshStopsRecycledName(t *testing.T) {
	mj := &fakeMojang{
		names:  map[string]string{normalize(notchUUID): "NotchNew"},
		owners: map[string]string{"notch": "11111111222233334444555555555555"},
	}
	l, p := openWith(t, "Notch:"+notchUUID+"\n", mj)

	// Before the refresh the file still says Notch, so the stranger gets in.
	if !l.Check("Notch", "", testIP) {
		t.Fatal("precondition: the stale name should still match")
	}

	l.Refresh()

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "NotchNew:"+notchUUID {
		t.Fatalf("file is %q, want the renamed entry", got)
	}
	// The name now belongs to someone else, and that someone is not listed.
	if l.Check("Notch", "", testIP) {
		t.Error("a released name was still honoured after the refresh")
	}
	// The real player, on the same old client, still gets in under the new name.
	if !l.Check("NotchNew", "", testIP) {
		t.Error("the renamed player was locked out")
	}
}

func TestRefreshLeavesUnchangedNamesAlone(t *testing.T) {
	mj := &fakeMojang{names: map[string]string{normalize(notchUUID): "Notch"}}
	l, p := openWith(t, "# friends\nNotch:"+notchUUID+"\n", mj)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	l.Refresh()
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("file rewritten though no name changed")
	}
}

// Mojang being unreachable must not empty the list or lock anyone out.
func TestRefreshSurvivesLookupFailure(t *testing.T) {
	mj := &fakeMojang{names: map[string]string{}} // every lookup misses
	l, _ := openWith(t, "Notch:"+notchUUID+"\n", mj)
	l.Refresh()
	if !l.Check("Notch", "", testIP) {
		t.Fatal("a failed refresh locked out a listed player")
	}
}

func TestRefreshStampGatesTheSchedule(t *testing.T) {
	mj := &fakeMojang{names: map[string]string{normalize(notchUUID): "Notch"}}
	l, p := openWith(t, "Notch:"+notchUUID+"\n", mj)

	// No stamp yet: a fresh node should refresh shortly after start.
	if d := l.untilNextRefresh(); d != settle {
		t.Fatalf("with no stamp, wait = %v, want %v", d, settle)
	}
	l.Refresh()

	// Stamped now, so the next one is a full interval out. A restart loop must not
	// turn into a burst of Mojang traffic.
	d := l.untilNextRefresh()
	if d < RefreshInterval-time.Minute || d > RefreshInterval {
		t.Fatalf("after a refresh, wait = %v, want about %v", d, RefreshInterval)
	}
	if _, err := os.Stat(p + stampSuffix); err != nil {
		t.Fatalf("stamp not written: %v", err)
	}
}

// Method B: a listed player who renamed and logged straight back in on a client too
// old to send a UUID, before the next refresh caught up.
func TestLookupAdmitsRenamedPlayerAndLearnsTheName(t *testing.T) {
	mj := &fakeMojang{owners: map[string]string{"notchnew": notchUUID}}
	l, p := openWith(t, "Notch:"+notchUUID+"\n", mj)

	if !l.Check("NotchNew", "", testIP) {
		t.Fatal("renamed player was refused")
	}
	if mj.uuidFor != 1 {
		t.Fatalf("UUIDFor called %d times, want 1", mj.uuidFor)
	}
	// The new name is recorded, so the next login matches locally and never asks
	// Mojang again.
	b, _ := os.ReadFile(p)
	if got := strings.TrimSpace(string(b)); got != "NotchNew:"+notchUUID {
		t.Fatalf("file is %q, want the learned name", got)
	}
	if !l.Check("NotchNew", "", testIP) {
		t.Fatal("second login refused")
	}
	if mj.uuidFor != 1 {
		t.Fatalf("UUIDFor called again (%d); the learned name should match locally", mj.uuidFor)
	}
}

func TestLookupRefusesStranger(t *testing.T) {
	mj := &fakeMojang{owners: map[string]string{"herobrine": "11111111222233334444555555555555"}}
	l, _ := openWith(t, "Notch:"+notchUUID+"\n", mj)

	if l.Check("Herobrine", "", testIP) {
		t.Fatal("a name owned by an unlisted uuid was admitted")
	}
}

// A refused lookup — rate limited, or Mojang down — must deny, never admit.
func TestLookupFailureDenies(t *testing.T) {
	mj := &fakeMojang{owners: map[string]string{}}
	l, _ := openWith(t, "Notch:"+notchUUID+"\n", mj)
	if l.Check("Herobrine", "", testIP) {
		t.Fatal("a failed lookup admitted a stranger")
	}
}

// A UUID the client supplied is decisive on its own; there is nothing for Mojang to
// add, and asking would hand a spoofer a free lookup.
func TestNoLookupWhenClientSentAUUID(t *testing.T) {
	mj := &fakeMojang{owners: map[string]string{"herobrine": notchUUID}}
	l, _ := openWith(t, "Notch:"+notchUUID+"\n", mj)

	if l.Check("Herobrine", "11111111-2222-3333-4444-555555555555", testIP) {
		t.Fatal("an unlisted uuid was admitted")
	}
	if mj.uuidFor != 0 {
		t.Fatalf("UUIDFor called %d times for a uuid-bearing login, want 0", mj.uuidFor)
	}
}

// A name that matched the file is trusted without a lookup: the refresh keeps that
// column fresh, and paying a round trip per login would defeat the point.
func TestNoLookupWhenNameMatches(t *testing.T) {
	mj := &fakeMojang{}
	l, _ := openWith(t, "Notch:"+notchUUID+"\n", mj)
	if !l.Check("Notch", "", testIP) {
		t.Fatal("listed name refused")
	}
	if mj.uuidFor != 0 || mj.nameFor != 0 {
		t.Fatalf("login hit Mojang (%d name, %d uuid lookups), want none", mj.nameFor, mj.uuidFor)
	}
}

// Two entries sharing a name make the name ambiguous, so it identifies nobody.
// UUID matching still works for both.
func TestDuplicateNameDisablesNameMatching(t *testing.T) {
	l, _ := open(t, "Notch:"+notchUUID+"\nNotch:"+alexUUID+"\n")
	if l.Check("Notch", "", testIP) {
		t.Error("an ambiguous name was admitted")
	}
	if !l.Check("Notch", notchUUID, testIP) || !l.Check("Notch", alexUUID, testIP) {
		t.Error("uuid matching should be unaffected by a duplicate name")
	}
}

func TestDuplicateUUIDRejected(t *testing.T) {
	if _, err := Open(write(t, "Notch:"+notchUUID+"\nAlt:"+notchUUID+"\n"), nil); err == nil {
		t.Fatal("the same uuid listed twice was accepted")
	}
}
