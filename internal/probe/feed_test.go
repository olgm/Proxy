package probe

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type postCapture struct {
	mu   sync.Mutex
	msgs []string
	url  string
}

func newPostCapture(t *testing.T) *postCapture {
	t.Helper()
	c := &postCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p struct {
			Content string `json:"content"`
		}
		json.Unmarshal(b, &p)
		c.mu.Lock()
		c.msgs = append(c.msgs, p.Content)
		c.mu.Unlock()
		w.Write([]byte(`{"id":"1"}`))
	}))
	t.Cleanup(srv.Close)
	c.url = srv.URL
	return c
}

func (c *postCapture) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.msgs, "\n")
}

// The dataset keeps every window; the channel gets the one worth reading. p99
// of a sixty-sample window is the second-worst of sixty, so the long window is
// the default.
func TestFeedWindowsDefaultToTheLongest(t *testing.T) {
	if got := FeedWindows([]string{"1m", "10m"}, nil); len(got) != 1 || got[0] != "10m" {
		t.Errorf("default = %v, want [10m]", got)
	}
	if got := FeedWindows([]string{"1m", "10m"}, []string{"1m"}); len(got) != 1 || got[0] != "1m" {
		t.Errorf("explicit = %v, want [1m]", got)
	}
	if got := FeedWindows(nil, nil); got != nil {
		t.Errorf("nothing configured = %v, want nothing", got)
	}
}

func TestFeedIsOffWithoutAURL(t *testing.T) {
	t.Setenv(EnvProbeWebhook, "")
	f, err := newFeed(Config{Name: "hk", Windows: []string{"1m"}})
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		t.Fatal("an unset URL produced a feed")
	}
	f.post(&Report{Sent: 1})
	f.Close()
}

func TestFeedPostsOnlyTheWindowsItWasGiven(t *testing.T) {
	c := newPostCapture(t)
	t.Setenv(EnvProbeWebhook, c.url)
	f, err := newFeed(Config{Name: "hk", Windows: []string{"1m", "10m"}})
	if err != nil {
		t.Fatal(err)
	}
	f.post(&Report{Class: "hk>ty", Kind: KindLeg, Dup: 1, Window: time.Minute, Sent: 60, Got: 60, Fwd: 60, N: 60})
	f.post(&Report{Class: "hk>ty", Kind: KindLeg, Dup: 2, Window: 10 * time.Minute, Sent: 600, Got: 600, Fwd: 600, N: 600,
		P50: 44.0, P90: 44.3, P99: 45.1, Mdev: 0.31})
	f.Close()

	all := c.all()
	if strings.Contains(all, "1m ·") {
		t.Errorf("posted a window it was not given:\n%s", all)
	}
	for _, want := range []string{"**hk**", "`hk>ty`", "×2", "10m", "p99 45.1ms", "loss 0.00%", "n 600"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q:\n%s", want, all)
		}
	}
}

// The first window of a series has no split and cannot: the count that arrived
// is the difference of two counters, and the earlier one comes from the window
// before. It must read as absent, not as zero.
func TestFeedOmitsTheSplitWhenThereIsNone(t *testing.T) {
	c := newPostCapture(t)
	t.Setenv(EnvProbeWebhook, c.url)
	f, _ := newFeed(Config{Name: "hk", Windows: []string{"1m"}})
	f.post(&Report{Class: "hk>ty", Kind: KindLeg, Dup: 1, Window: time.Minute, Sent: 60, Got: 60, Fwd: -1, N: 60})
	f.Close()

	all := c.all()
	if strings.Contains(all, "out ") || strings.Contains(all, "back ") {
		t.Errorf("invented a direction split:\n%s", all)
	}
	if !strings.Contains(all, "loss 0.00%") {
		t.Errorf("round-trip loss is knowable and should still be there:\n%s", all)
	}
}

// End to end over a real socket: what probed measures is what reaches the
// channel, under the same name the dataset files it under.
func TestFeedPostsWhatWasMeasured(t *testing.T) {
	c := newPostCapture(t)
	t.Setenv(EnvProbeWebhook, c.url)

	k := key()
	bindAddr, respAddr := bind(t)
	resp := Config{Name: "resp", Bind: bindAddr, Hz: 400, Windows: []string{"200ms"},
		TimeoutMS: 150, Links: []LinkConfig{{Addr: "127.0.0.1", Key: k}},
		Classes: []Class{{ID: 1, Name: "a>b", Kind: KindLeg, Duplicate: 1, Up: []Hop{{Link: 0}}}}}
	path := logPath(t)
	orig := fast("orig", path)
	orig.Links = []LinkConfig{{Addr: respAddr.String(), Key: k}}
	orig.Classes = []Class{{ID: 1, Name: "a>b", Kind: KindLeg, Duplicate: 1, Down: []Hop{{Link: 0}}}}

	mustNode(t, resp)
	n := mustNode(t, orig)

	// Wait for the dataset to have a window, then close: closing flushes the
	// feed, which beats waiting out its gathering interval.
	awaitOne(t, path, func(l line) bool { return l.N > 10 })
	n.Close()

	all := c.all()
	if !strings.Contains(all, "**orig**") || !strings.Contains(all, "`a>b`") {
		t.Fatalf("the measurement did not reach the channel:\n%s", all)
	}
	if !strings.Contains(all, "200ms") {
		t.Errorf("window is not the one that was measured:\n%s", all)
	}
}
