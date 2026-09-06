package main

import (
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// A refused connection is the kernel saying the host is here and nothing is on
// that port. Silence is the host itself being gone. Reporting those the same way
// would lose the distinction the status feed exists for.
func TestClassifyTellsARefusalFromSilence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want reach
	}{
		{"nothing wrong", nil, reachOK},
		{"refused", &net.OpError{Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, reachRefused},
		{"host unreachable", &net.OpError{Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}, reachUnreachable},
		{"timed out", &net.OpError{Err: &timeoutErr{}}, reachUnreachable},
		{"talked, then failed", errors.New("reply does not open"), reachBroken},
	} {
		if got := classify(tc.err); got != tc.want {
			t.Errorf("%s: classify = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type timeoutErr struct{}

func (t *timeoutErr) Error() string   { return "i/o timeout" }
func (t *timeoutErr) Timeout() bool   { return true }
func (t *timeoutErr) Temporary() bool { return true }

func testWatcher() (*watcher, *[]string) {
	var posted []string
	w := &watcher{
		state: map[string]string{}, strikes: map[string]int{}, seen: map[string]bool{},
	}
	w.send = func(line string) { posted = append(posted, line) }
	return w, &posted
}

// One dropped packet is not an outage. A change has to hold for several dials
// before anyone is told about it.
func TestStatusWaitsForAChangeToHold(t *testing.T) {
	w, posted := testWatcher()
	w.observe("hk", "") // baseline: healthy, and silent about it

	for i := 0; i < statusStrikes-1; i++ {
		w.observe("hk", "proxyd is down")
	}
	if len(*posted) != 0 {
		t.Fatalf("announced before the change held: %v", *posted)
	}
	w.observe("hk", "proxyd is down")
	if len(*posted) != 1 || !strings.Contains((*posted)[0], "proxyd is down") {
		t.Fatalf("did not announce a held change: %v", *posted)
	}

	// And a flap that goes back before the strikes run out says nothing.
	w.observe("hk", "")
	w.observe("hk", "proxyd is down")
	if len(*posted) != 1 {
		t.Fatalf("a flap was announced: %v", *posted)
	}
}

// A node that is already down when the bot starts is news, so the first
// observation is announced when it is a fault — and only then.
func TestStatusAnnouncesAFaultItStartedWith(t *testing.T) {
	w, posted := testWatcher()
	w.observe("hk", "unreachable")
	if len(*posted) != 1 || !strings.Contains((*posted)[0], "unreachable") {
		t.Fatalf("a node already down at startup was not reported: %v", *posted)
	}

	w2, quiet := testWatcher()
	w2.observe("hk", "")
	if len(*quiet) != 0 {
		t.Fatalf("a healthy node announced itself at startup: %v", *quiet)
	}
}

func TestStatusAnnouncesRecovery(t *testing.T) {
	w, posted := testWatcher()
	w.observe("hk", "proxyd is down")
	for i := 0; i < statusStrikes; i++ {
		w.observe("hk", "")
	}
	if len(*posted) != 2 || !strings.Contains((*posted)[1], "recovered") {
		t.Fatalf("recovery not announced: %v", *posted)
	}
}

// An unreachable node must report once, not once per service on it: probed is
// not asked about when the node itself did not answer.
func TestUnreachableNodeReportsOnce(t *testing.T) {
	w, posted := testWatcher()
	w.observe("hk", "unreachable — no answer from the node at all")
	if len(*posted) != 1 {
		t.Fatalf("want one line for one fault, got %v", *posted)
	}
	if strings.Count((*posted)[0], "probed") != 0 {
		t.Errorf("probed was reported for an unreachable node: %v", *posted)
	}
}
