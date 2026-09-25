package webhook

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

// capture is a stand-in Discord: it records every body posted to it.
type capture struct {
	mu     sync.Mutex
	bodies []payload
	paths  []string
	hold   chan struct{} // when non-nil, the first request waits on it
	status int
	held   bool
}

func (c *capture) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var p payload
	json.Unmarshal(b, &p)

	c.mu.Lock()
	c.bodies = append(c.bodies, p)
	c.paths = append(c.paths, r.URL.Path+"?"+r.URL.RawQuery)
	hold, first := c.hold, !c.held
	c.held = true
	status := c.status
	c.mu.Unlock()

	if hold != nil && first {
		<-hold
	}
	if status != 0 {
		w.WriteHeader(status)
		w.Write([]byte(`{}`))
		return
	}
	w.Write([]byte(`{"id":"991"}`))
}

func (c *capture) sent() []payload {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]payload(nil), c.bodies...)
}

func newCapture(t *testing.T) (*capture, string) {
	t.Helper()
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(c.handler))
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func TestPostReturnsMessageID(t *testing.T) {
	c, url := newCapture(t)
	id, err := New(url).Post("hello")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if id != "991" {
		t.Fatalf("id = %q, want 991", id)
	}
	sent := c.sent()
	if len(sent) != 1 || sent[0].Content != "hello" {
		t.Fatalf("sent %+v", sent)
	}
	// The id is only returned when Discord is asked to wait for the message.
	if !strings.Contains(c.paths[0], "wait=true") {
		t.Errorf("post did not ask to wait: %s", c.paths[0])
	}
}

// A feed is a record. An IGN or a tag that looks like a mention must not ping a
// channel every time that player logs in.
func TestPostSuppressesMentions(t *testing.T) {
	c, url := newCapture(t)
	if _, err := New(url).Post("@everyone joined"); err != nil {
		t.Fatalf("post: %v", err)
	}
	got := c.sent()[0].AllowedMentions.Parse
	if got == nil || len(got) != 0 {
		t.Fatalf("allowed_mentions.parse = %#v, want an empty list", got)
	}
}

func TestEditTargetsTheMessage(t *testing.T) {
	c, url := newCapture(t)
	if err := New(url).Edit("991", "now this"); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.HasSuffix(c.paths[0], "/messages/991?") {
		t.Fatalf("edit went to %s", c.paths[0])
	}
	if c.sent()[0].Content != "now this" {
		t.Fatalf("sent %+v", c.sent())
	}
}

// A roster message somebody deleted is not an error to report and give up on:
// it is the cue to post a new one.
func TestEditGoneReportsErrGone(t *testing.T) {
	c, url := newCapture(t)
	c.status = http.StatusNotFound
	if err := New(url).Edit("991", "x"); err != ErrGone {
		t.Fatalf("err = %v, want ErrGone", err)
	}
}

func TestRetriesA429(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"retry_after":0.01}`))
			return
		}
		w.Write([]byte(`{"id":"1"}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL).Post("x"); err != nil {
		t.Fatalf("post: %v", err)
	}
	if n != 2 {
		t.Fatalf("made %d requests, want 2", n)
	}
}

// The URL is the credential. It must not turn up in an error a node will log.
func TestErrorsDoNotLeakTheURL(t *testing.T) {
	url := "http://127.0.0.1:1/webhooks/12345/s3cr3t-token"
	_, err := New(url).Post("x")
	if err == nil {
		t.Fatal("want an error from an unreachable webhook")
	}
	if strings.Contains(err.Error(), "s3cr3t-token") || strings.Contains(err.Error(), "webhooks/12345") {
		t.Fatalf("error carries the webhook URL: %v", err)
	}
}

// Several logins in one flush are one message, not one message each. That is
// both what keeps a reconnect storm inside Discord's rate limit and what makes
// the channel readable.
func TestQueueCoalesces(t *testing.T) {
	c, url := newCapture(t)
	q := NewQueue(url, 16, 10*time.Millisecond)
	q.Send("one")
	q.Send("two")
	q.Send("three")
	q.Close()

	sent := c.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1: %+v", len(sent), sent)
	}
	if sent[0].Content != "one\ntwo\nthree" {
		t.Fatalf("content = %q", sent[0].Content)
	}
}

// A feed that blocks a login is worse than a feed with a hole in it, so a full
// queue drops — and says so, rather than leaving a silent gap.
func TestQueueDropsAndSaysSo(t *testing.T) {
	c, url := newCapture(t)
	c.hold = make(chan struct{})

	q := NewQueue(url, 2, 5*time.Millisecond)
	q.Send("first")
	// Wait for the sender to be inside that first post, holding the channel.
	for len(c.sent()) == 0 {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		q.Send("more")
	}
	close(c.hold)
	q.Close()

	var joined []string
	for _, p := range c.sent() {
		joined = append(joined, p.Content)
	}
	all := strings.Join(joined, "\n")
	if !strings.Contains(all, "did not fit") {
		t.Fatalf("no drop note in %q", all)
	}
}

// A single line past Discord's limit is truncated rather than rejected: losing
// the tail of one logout line is better than losing the line.
func TestOversizeLineIsTruncated(t *testing.T) {
	c, url := newCapture(t)
	if _, err := New(url).Post(strings.Repeat("x", maxContent+500)); err != nil {
		t.Fatalf("post: %v", err)
	}
	if n := len(c.sent()[0].Content); n > maxContent {
		t.Fatalf("sent %d bytes, over the %d limit", n, maxContent)
	}
}

// A node shuts its feed down while sessions are still ending, so Close races
// Send by construction. Closing the channel to signal it would turn that race
// into "send on closed channel" and take the process down — which is the one
// thing a feed must never do to the thing it reports on.
func TestSendDuringCloseIsSafe(t *testing.T) {
	_, url := newCapture(t)
	q := NewQueue(url, 8, time.Millisecond)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				q.Send("line")
			}
		}()
	}
	go q.Close()
	wg.Wait()
	q.Close() // idempotent, and the second caller waits like the first
}

// What was queued a moment before the node went down should still get out.
func TestCloseFlushesWhatWasQueued(t *testing.T) {
	c, url := newCapture(t)
	q := NewQueue(url, 16, time.Hour) // never ticks: only Close can flush this
	q.Send("last words")
	q.Close()

	if all := c.sent(); len(all) != 1 || all[0].Content != "last words" {
		t.Fatalf("close did not flush the queue: %+v", all)
	}
}

// A queue detached for a handoff posts nothing and hands back what it had, in
// order, for the next process to send: posting it here and there would say it
// twice, and dropping it would lose a logout.
func TestDetachHandsBackWhatWasQueued(t *testing.T) {
	c, url := newCapture(t)
	q := NewQueue(url, 16, time.Hour)
	q.Send("one")
	q.Send("two")
	left := q.Detach(time.Second)
	if strings.Join(left, ",") != "one,two" {
		t.Fatalf("handed back %q", left)
	}
	q.Send("after")
	q.Close()
	if all := c.sent(); len(all) != 0 {
		t.Fatalf("a detached queue posted: %+v", all)
	}
}

// A drop that happens while nothing else is queued still has to be reported on
// its own: if the note only ever rode along with a message that was already
// going out, a drop with nothing left to carry it — the sender busy, everything
// it had already posted, and the lines turned away the last of them — would take
// the count away with it, and the feed would look quiet rather than incomplete.
func TestADropIsReportedEvenWithNothingElseToSay(t *testing.T) {
	c, url := newCapture(t)
	q := NewQueue(url, 4, 5*time.Millisecond)
	q.drops.Add(7)
	q.Close()

	var all []string
	for _, p := range c.sent() {
		all = append(all, p.Content)
	}
	joined := strings.Join(all, "\n")
	if !strings.Contains(joined, "did not fit") || !strings.Contains(joined, "7") {
		t.Fatalf("seven dropped lines went unreported: %q", joined)
	}
}

// A queue that is filling up posts early rather than sitting on its timer until
// it overflows. The room only ever runs short because the sender was slow to use
// it, and a line dropped for want of room is a login nobody will see reported.
func TestAFillingQueuePostsBeforeItOverflows(t *testing.T) {
	c, url := newCapture(t)

	// A flush interval far longer than the test: anything that goes out went out
	// because the queue was filling, not because the timer fired.
	q := NewQueue(url, 8, time.Hour)
	for i := 0; i < 8; i++ {
		q.Send("line")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(c.sent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a filling queue waited for its timer instead of posting")
		}
		time.Sleep(time.Millisecond)
	}
	q.Close()
	if q.drops.Load() != 0 {
		t.Errorf("%d lines were dropped by a queue that had room to post", q.drops.Load())
	}
}

// A handoff that lands while a post is on its way to Discord does not wait for
// it, but it still takes what was queued behind it: those lines are not in that
// post, and once the process has gone nothing else would send them.
func TestDetachDuringAPostHandsBackWhatWaitedBehindIt(t *testing.T) {
	c, url := newCapture(t)
	c.hold = make(chan struct{})
	defer close(c.hold)
	q := NewQueue(url, 16, 10*time.Millisecond)
	q.Send("first")
	deadline := time.Now().Add(5 * time.Second)
	for len(c.sent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing was posted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	q.Send("second")
	if left := q.Detach(50 * time.Millisecond); strings.Join(left, ",") != "second" {
		t.Fatalf("handed back %q, want the line queued behind the post", left)
	}
}
