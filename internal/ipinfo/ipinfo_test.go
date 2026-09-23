package ipinfo

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPrefix(t *testing.T) {
	for ip, want := range map[string]string{
		"203.0.113.77":          "203.0.113.0/24",
		"::ffff:203.0.113.77":   "203.0.113.0/24",
		"2001:db8:1234:5678::1": "2001:db8:1234::/48",
		"127.0.0.1":             "",
		"10.1.2.3":              "",
		"192.168.1.1":           "",
		"100.100.1.1":           "", // shared space, where a tailnet lives
		"fe80::1":               "",
		"not an address":        "",
	} {
		got := ""
		if p, ok := Prefix(ip); ok {
			got = p.String()
		}
		if got != want {
			t.Errorf("Prefix(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestInfoString(t *testing.T) {
	for _, c := range []struct {
		in   Info
		want string
	}{
		{Info{City: "Seoul", Region: "Seoul", Country: "KR", Org: "AS4766 Korea Telecom"}, "Seoul, KR · AS4766 Korea Telecom"},
		{Info{City: "Melbourne", Region: "Victoria", Country: "AU"}, "Melbourne, Victoria, AU"},
		{Info{Org: "AS4766 Korea Telecom"}, "AS4766 Korea Telecom"},
		{Info{Net: "203.0.113.0/24"}, "unknown"},
	} {
		if got := c.in.String(); got != c.want {
			t.Errorf("%+v rendered %q, want %q", c.in, got, c.want)
		}
	}
}

// fakeIPInfo answers every lookup with Seoul, or with status when it is not 200,
// and remembers every path it was asked for.
func fakeIPInfo(t *testing.T, status int) (*Client, func() []string) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		ip := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/json")
		w.Write([]byte(`{"ip":"` + ip + `","city":"Seoul","region":"Seoul","country":"KR","org":"AS4766 Korea Telecom"}`))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL + "/"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func waitFor(t *testing.T, c *Client, ip string) *Info {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info := c.Get(ip); info != nil {
			return info
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no answer for %s", ip)
	return nil
}

// Two players in one /24 cost one question, and the question names the /24, never
// either of them.
func TestWatchAsksOncePerPrefix(t *testing.T) {
	c, asked := fakeIPInfo(t, http.StatusOK)
	c.Watch("203.0.113.77")
	c.Watch("203.0.113.77")
	info := waitFor(t, c, "203.0.113.77")
	c.Watch("203.0.113.200")
	if got := c.Get("203.0.113.200"); got == nil || *got != *info {
		t.Errorf("a neighbour in the same /24 got %+v, want %+v", got, info)
	}
	if info.Net != "203.0.113.0/24" || info.Country != "KR" || info.Org != "AS4766 Korea Telecom" {
		t.Errorf("answer kept wrong: %+v", info)
	}
	if got := asked(); len(got) != 1 || got[0] != "/203.0.113.0/json" {
		t.Errorf("asked %q, want one question about the /24", got)
	}
	if c.Get("198.51.100.1") != nil {
		t.Error("a prefix nobody asked about came back known")
	}
}

// An address that is not worth asking about is never asked about, and a nil
// client — lookups off — knows nothing and does nothing.
func TestWatchSkipsWhatItShould(t *testing.T) {
	c, asked := fakeIPInfo(t, http.StatusOK)
	c.Watch("127.0.0.1")
	c.Watch("100.100.1.1")
	time.Sleep(50 * time.Millisecond)
	if got := asked(); len(got) != 0 {
		t.Errorf("asked about %q", got)
	}
	var off *Client
	off.Watch("203.0.113.77")
	if off.Get("203.0.113.77") != nil {
		t.Error("a nil client knew something")
	}
}

// A failed lookup is not kept, and is not retried by every login either: the
// prefix waits retryAfter, then the next login from it asks again.
func TestFailedLookupWaitsBeforeAskingAgain(t *testing.T) {
	c, asked := fakeIPInfo(t, http.StatusTooManyRequests)
	c.Watch("203.0.113.77")
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		failed := !c.nets[mustPrefix(t, "203.0.113.77")].failed.IsZero()
		c.mu.Unlock()
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the lookup never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.Watch("203.0.113.77")
	if n := len(asked()); n != 1 {
		t.Fatalf("asked %d times straight after a failure, want 1", n)
	}
	if c.Get("203.0.113.77") != nil {
		t.Error("a failed lookup came back as an answer")
	}

	old := retryAfter
	retryAfter = 0
	t.Cleanup(func() { retryAfter = old })
	c.Watch("203.0.113.77")
	deadline = time.Now().Add(5 * time.Second)
	for len(asked()) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the prefix was never asked about again")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mustPrefix(t *testing.T, ip string) netip.Prefix {
	t.Helper()
	p, ok := Prefix(ip)
	if !ok {
		t.Fatalf("no prefix for %s", ip)
	}
	return p
}
