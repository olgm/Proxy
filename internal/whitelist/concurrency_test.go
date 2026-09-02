package whitelist

import (
	"testing"
	"time"
)

// blockingMojang holds a lookup open until released, standing in for the 650 ms
// (or 5 s, if Mojang is down) that a real one takes.
type blockingMojang struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingMojang) NameFor(uuid string) (string, bool) { return "", false }

func (b *blockingMojang) UUIDFor(name, ip string) (string, bool) {
	b.entered <- struct{}{}
	<-b.release
	return "", false
}

// A lookup must not hold the list. Every login on an ingress goes through Check, so
// a stranger's miss blocking them all would turn one slow Mojang call into a stall
// for everybody — and at the global rate limit that is most of the time.
func TestLookupDoesNotBlockOtherLogins(t *testing.T) {
	mj := &blockingMojang{entered: make(chan struct{}), release: make(chan struct{})}
	l, _ := openWith(t, "Notch:"+notchUUID+"\n", mj)
	defer close(mj.release)

	// A miss, which will sit inside the Mojang call until released.
	go l.Check("Stranger", "", "198.51.100.9")
	<-mj.entered

	// Meanwhile a listed player logs in. This is answered from the file and must
	// not wait on the lookup above.
	done := make(chan bool, 1)
	go func() { done <- l.Check("Notch", notchUUID, testIP) }()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("listed player refused")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a listed player's login blocked behind a stranger's Mojang lookup")
	}
}

// Two misses for the same name must not serialise on the list either.
func TestConcurrentLookupsDoNotSerialiseOnTheList(t *testing.T) {
	mj := &blockingMojang{entered: make(chan struct{}, 2), release: make(chan struct{})}
	l, _ := openWith(t, "Notch:"+notchUUID+"\n", mj)
	defer close(mj.release)

	go l.Check("StrangerA", "", "198.51.100.9")
	go l.Check("StrangerB", "", "198.51.100.10")

	for i := 0; i < 2; i++ {
		select {
		case <-mj.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("second lookup never started; they are serialised on the list")
		}
	}
}
