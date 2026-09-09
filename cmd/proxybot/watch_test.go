package main

import (
	"strings"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/control"
)

const (
	watched   = "user-watched"
	alexUUID2 = "853c80ef-3c37-49fd-aa49-938b674adae6"
)

func manager() member { return member{id: "user-manager", roles: []string{managerRole}} }

// pastAt builds a finished session that ended n minutes ago.
func pastAt(node, name, uuid string, minsAgo int) control.Past {
	end := time.Now().Add(-time.Duration(minsAgo) * time.Minute)
	return control.Past{
		Node: node, Name: name, UUID: uuid, IP: "203.0.113.9",
		Start: end.Add(-10 * time.Minute), End: end,
		Up: 4200, Down: 51700, Chain: 111600,
	}
}

// watchBot gives the watched member two accounts, listed on both entries.
func watchBot(t *testing.T) (*bot, *fakeNode, *fakeNode) {
	t.Helper()
	b, hk, ty, _ := newBot(t)
	line := "Notch:" + notchUUID + " # " + tagFor(watched) + "\n" +
		"Alex:" + alexUUID2 + " # " + tagFor(watched) + "\n"
	hk.write(t, line)
	ty.write(t, line)
	return b, hk, ty
}

// The reply carries a client IP, so only a manager may ask for it.
func TestWatchIsManagersOnly(t *testing.T) {
	b, _, _ := watchBot(t)
	r := b.watch(member{id: "someone", roles: []string{memberRole}}, watched, 0)
	if !strings.Contains(r.text, "Only managers") {
		t.Fatalf("a plain member was answered: %q", r.text)
	}
	if strings.Contains(r.text, "203.0.113.9") {
		t.Fatal("a refusal leaked a client IP")
	}

	none := b.watch(member{id: "nobody"}, watched, 0)
	if !strings.Contains(none.text, "no role") {
		t.Fatalf("a member with no role was answered: %q", none.text)
	}
}

func TestWatchShowsAccountsOnlineAndHistory(t *testing.T) {
	b, hk, ty := watchBot(t)
	hk.sess.set(
		[]control.Live{{Name: "Notch", UUID: notchUUID, IP: "203.0.113.9", Since: time.Now().Add(-12 * time.Minute)}},
		[]control.Past{pastAt("", "Notch", notchUUID, 90)},
	)
	ty.sess.set(nil, []control.Past{pastAt("", "Alex", alexUUID2, 30)})

	r := b.watch(manager(), watched, 0)
	for _, want := range []string{
		"Watching <@" + watched + ">",
		"**Accounts**", "`Notch`", "`Alex`", notchUUID,
		"**Online now**", "on **hk**", "203.0.113.9", "12m",
		"**Sessions**", "up 4.1KB", "chain 109.0KB",
	} {
		if !strings.Contains(r.text, want) {
			t.Errorf("watch is missing %q:\n%s", want, r.text)
		}
	}
	// Newest first, across nodes: Alex ended 30 minutes ago, Notch 90.
	if strings.Index(r.text, "`Alex` **ty**") > strings.Index(r.text, "`Notch` **hk**") {
		t.Errorf("sessions are not newest first:\n%s", r.text)
	}
}

// A member with no accounts has no sessions to look up, and asking the nodes
// for the history of an empty set would return everybody's.
func TestWatchWithNoAccountsAsksForNothing(t *testing.T) {
	b, hk, ty := watchBot(t)
	hk.write(t, "# empty\n")
	ty.write(t, "# empty\n")
	hk.sess.set(nil, []control.Past{pastAt("", "Somebody", "11111111-1111-1111-1111-111111111111", 5)})

	r := b.watch(manager(), watched, 0)
	if !strings.Contains(r.text, "none") {
		t.Fatalf("no accounts should say so:\n%s", r.text)
	}
	if strings.Contains(r.text, "Somebody") {
		t.Fatalf("watch showed a session belonging to nobody it asked about:\n%s", r.text)
	}
}

func TestWatchPagesThroughSessions(t *testing.T) {
	b, hk, _ := watchBot(t)
	var past []control.Past
	for i := 0; i < watchPage*2+1; i++ {
		// Oldest first, as a log file holds them.
		past = append(past, pastAt("", "Notch", notchUUID, 100-i))
	}
	hk.sess.set(nil, past)

	first := b.watch(manager(), watched, 0)
	if !first.more {
		t.Fatal("first page does not offer another")
	}
	if n := strings.Count(first.text, "`Notch` **hk**"); n != watchPage {
		t.Fatalf("first page holds %d sessions, want %d", n, watchPage)
	}
	second := b.watch(manager(), watched, 1)
	if !strings.Contains(second.text, "page 2") {
		t.Errorf("second page is not labelled:\n%s", second.text)
	}
	last := b.watch(manager(), watched, 2)
	if last.more {
		t.Fatal("the last page offers another")
	}
	// And every session appears exactly once across the three pages.
	total := strings.Count(first.text+second.text+last.text, "`Notch` **hk**")
	if total != len(past) {
		t.Fatalf("%d sessions across the pages, want %d", total, len(past))
	}
}

// The page rides in the button's own id, so the bot remembers nothing between
// one press and the next.
func TestWatchButtonIDRoundTrips(t *testing.T) {
	user, page, ok := parseWatchID(watchID("123", 4))
	if !ok || user != "123" || page != 4 {
		t.Fatalf("round trip gave %q %d %v", user, page, ok)
	}
	for _, bad := range []string{"", "other:1", watchPrefix + "123", watchPrefix + ":2", watchPrefix + "123:x", watchPrefix + "123:-1"} {
		if _, _, ok := parseWatchID(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

// A session line is where the client IP is reported, deliberately: one manager
// reading a private reply is not the audience a channel feed is.
func TestSessionLineCarriesWhatWasAskedFor(t *testing.T) {
	got := sessionLine(pastAt("hk", "Notch", notchUUID, 0))
	// Bytes, not a ratio. What a session cost is a figure to add up across a
	// month; how many times its own size that was is arithmetic the reader can
	// do and mostly does not want.
	if strings.Contains(got, "×") {
		t.Errorf("session line still carries a multiple: %s", got)
	}
	for _, want := range []string{"`Notch`", "**hk**", "`203.0.113.9`", "up 4.1KB", "down 50.5KB", "chain 109.0KB"} {
		if !strings.Contains(got, want) {
			t.Errorf("session line is missing %q: %s", want, got)
		}
	}
}
