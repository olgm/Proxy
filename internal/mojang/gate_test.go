package mojang

import (
	"fmt"
	"testing"
	"time"
)

// testGate is a Gate on a clock the test drives.
func testGate() (*Gate, *time.Time) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	g := NewGate()
	g.now = func() time.Time { return now }
	return g, &now
}

// allowN reports how many of n consecutive lookups the gate permits.
func allowN(g *Gate, name, ip string, n int) int {
	got := 0
	for i := 0; i < n; i++ {
		if g.Allow(name, ip) {
			got++
		}
	}
	return got
}

// The first rung: three lookups per five minutes for one name.
func TestFirstRung(t *testing.T) {
	g, _ := testGate()
	if got := allowN(g, "Notch", "198.51.100.7", 5); got != 3 {
		t.Fatalf("allowed %d of 5, want 3", got)
	}
}

// Spending a rung denies that request and moves the key down, with the longer
// window starting already spent. Hitting a limit means waiting, not being handed
// fresh budget.
func TestEscalationCostsTheRestOfTheWindow(t *testing.T) {
	g, now := testGate()
	allowN(g, "Notch", "198.51.100.7", 4) // 3 allowed, 4th escalates to 2/hr

	// Still inside the new hour: nothing more, however long they keep trying.
	*now = now.Add(30 * time.Minute)
	if got := allowN(g, "Notch", "198.51.100.7", 10); got != 0 {
		t.Fatalf("allowed %d during the penalty window, want 0", got)
	}

	// Hour rolls, and they were still trying, so the rung stands: 2 per hour.
	*now = now.Add(31 * time.Minute)
	if got := allowN(g, "Notch", "198.51.100.7", 5); got != 2 {
		t.Fatalf("allowed %d on the second rung, want 2", got)
	}
}

// Ladder bottom: 3/5min -> 2/hr -> 1/hr, and the last rung repeats rather than
// expiring, so sustained pressure stays throttled.
func TestLadderBottomsOutAndRepeats(t *testing.T) {
	g, now := testGate()
	const name, ip = "Notch", "198.51.100.7"

	// Sustained pressure: the key keeps asking through every window. Going quiet
	// instead is what decays a rung, so reaching the bottom means never letting up.
	allowN(g, name, ip, 4) // 3 allowed, 4th -> rung 1 (2/hr)
	*now = now.Add(30 * time.Minute)
	allowN(g, name, ip, 2) // refused, but keeps the key from reading as idle

	*now = now.Add(31 * time.Minute)
	if got := allowN(g, name, ip, 3); got != 2 { // 2 allowed, 3rd -> rung 2 (1/hr)
		t.Fatalf("second rung allowed %d, want 2", got)
	}

	for i := 0; i < 3; i++ {
		*now = now.Add(30 * time.Minute)
		allowN(g, name, ip, 1) // keep the pressure on
		*now = now.Add(31 * time.Minute)
		if got := allowN(g, name, ip, 4); got != 1 {
			t.Fatalf("hour %d: allowed %d, want 1 on the bottom rung", i, got)
		}
	}
}

// A window that elapses with no attempt at all eases off one rung. Someone who
// renamed, retried a few times and went away is not pinned for the rest of the day.
func TestIdleWindowDecaysOneRung(t *testing.T) {
	g, now := testGate()
	const name, ip = "Notch", "198.51.100.7"

	allowN(g, name, ip, 4) // -> rung 1 (2/hr)

	// Sit out the whole hour without asking for anything.
	*now = now.Add(61 * time.Minute)
	// First rung again: three per five minutes.
	if got := allowN(g, name, ip, 5); got != 3 {
		t.Fatalf("allowed %d after an idle window, want 3", got)
	}
}

// The name and IP limiters are independent, so one burned name does not lock an
// address out and one address cannot spend every name's budget.
func TestNameAndIPAreSeparate(t *testing.T) {
	g, _ := testGate()
	// Burn one name from one address.
	allowN(g, "Notch", "198.51.100.7", 4)

	// A different name from a fresh address is untouched.
	if got := allowN(g, "Alex", "198.51.100.9", 3); got != 3 {
		t.Fatalf("allowed %d for an unrelated name and ip, want 3", got)
	}
}

// One address cycling names is caught by the IP limiter even though every name is
// new. This is the case the per-name limit alone would miss.
func TestOneAddressCyclingNamesIsCaught(t *testing.T) {
	g, _ := testGate()
	got := 0
	for i := 0; i < 10; i++ {
		if g.Allow(fmt.Sprintf("Name%d", i), "198.51.100.7") {
			got++
		}
	}
	if got != 3 {
		t.Fatalf("allowed %d lookups from one address, want 3", got)
	}
}

// A request refused by one limiter must not spend the others. Here the address is
// exhausted by other names, so the name being refused has spent nothing itself and
// must arrive at a clean address with its full first rung.
func TestRefusalDoesNotBurnOtherLimiters(t *testing.T) {
	g, _ := testGate()
	allowN(g, "Alex", "198.51.100.7", 1)
	allowN(g, "Steve", "198.51.100.7", 1)
	allowN(g, "Herobrine", "198.51.100.7", 1) // address now spent

	if g.Allow("Notch", "198.51.100.7") {
		t.Fatal("the exhausted address was allowed a lookup")
	}
	if got := allowN(g, "Notch", "198.51.100.9", 5); got != 3 {
		t.Fatalf("allowed %d, want 3: a name refused on the ip limiter was charged anyway", got)
	}
}

// The global cap bounds what we can ever send Mojang, whoever is asking.
func TestGlobalCap(t *testing.T) {
	g, _ := testGate()
	got := 0
	// Fresh name and address every time, so only the global cap can bite.
	for i := 0; i < globalN*2; i++ {
		if g.Allow(fmt.Sprintf("Name%d", i), fmt.Sprintf("198.51.%d.%d", i/256, i%256)) {
			got++
		}
	}
	if got != globalN {
		t.Fatalf("allowed %d lookups overall, want %d", got, globalN)
	}
}

func TestGlobalCapRollsOver(t *testing.T) {
	g, now := testGate()
	for i := 0; i < globalN; i++ {
		g.Allow(fmt.Sprintf("Name%d", i), fmt.Sprintf("198.51.%d.%d", i/256, i%256))
	}
	*now = now.Add(globalWindow + time.Second)
	if !g.Allow("Fresh", "198.51.100.7") {
		t.Fatal("global cap did not roll over")
	}
}

// A globally refused request must not escalate the name or IP: they did nothing
// wrong, and the ladder is for keys that keep missing.
func TestGlobalRefusalDoesNotEscalate(t *testing.T) {
	g, now := testGate()
	for i := 0; i < globalN; i++ {
		g.Allow(fmt.Sprintf("Name%d", i), fmt.Sprintf("198.51.%d.%d", i/256, i%256))
	}
	for i := 0; i < 5; i++ {
		g.Allow("Notch", "203.0.113.5") // all refused globally
	}
	*now = now.Add(globalWindow + time.Second)
	if got := allowN(g, "Notch", "203.0.113.5", 5); got != 3 {
		t.Fatalf("allowed %d after the global window rolled, want 3 (still on the first rung)", got)
	}
}
