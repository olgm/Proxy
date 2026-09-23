package proxy

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/jsonl"
	"github.com/olgm/proxy/internal/mc"
)

func TestClientRTTSummary(t *testing.T) {
	var w clientRTT
	if w.summary() != nil {
		t.Fatal("a session with no readings claimed a round trip")
	}
	// Ten readings, 30..39 ms smoothed and 1..10 ms of deviation, in no order.
	for _, i := range []int{3, 9, 0, 5, 1, 8, 2, 7, 4, 6} {
		w.rtt = append(w.rtt, time.Duration(30+i)*time.Millisecond)
		w.rttvar = append(w.rttvar, time.Duration(1+i)*time.Millisecond)
	}
	w.min, w.retrans = 27340*time.Microsecond, 4
	got := *w.summary()
	want := control.RTT{Min: 27.3, P50: 34, P90: 38, Max: 39, Var: 5, Retrans: 4, N: 10}
	if got != want {
		t.Errorf("summary = %+v\nwant      %+v", got, want)
	}
	if s := rttPart(&got); s != " rtt=27.3/34.0/38.0/39.0ms rttvar=5.0ms retrans=4" {
		t.Errorf("logout line part = %q", s)
	}
	if s := rttPart(nil); s != "" {
		t.Errorf("no round trip rendered as %q", s)
	}
}

// The client leg is written down with the session. Loopback's round trip rounds
// to nothing, so this asserts it was read, not what it was.
func TestFinishedSessionCarriesClientRTT(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("TCP_INFO is read on Linux only")
	}
	path := filepath.Join(t.TempDir(), "sessions.jsonl")
	w, err := jsonl.NewWriter(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	backend, got := fakeLoginBackend(t)
	ingress, s := feedIngress(t, backend, whitelistFile(t, "Notch:"+notchUUID+"\n"), "")
	s.live.log, s.live.path = w, path

	c := dialIngress(t, ingress, 764, mc.IntentLogin, loginStart("Notch", notchRaw[:]))
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("login never reached the backend")
	}
	c.Close()
	for s.online.Load() != 0 {
		time.Sleep(5 * time.Millisecond)
	}

	past, err := s.live.History(nil, 10)
	if err != nil || len(past) != 1 {
		t.Fatalf("history: %d sessions, %v", len(past), err)
	}
	if r := past[0].RTT; r == nil || r.N < 1 {
		t.Errorf("session written down with no client round trip: %+v", r)
	}
}
