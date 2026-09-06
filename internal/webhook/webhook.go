// Package webhook posts a feed to a Discord webhook.
//
// It is stdlib only on purpose. proxyd and probed both post their own feeds and
// both are dependency-free binaries on a node; proxybot has discordgo already
// and uses this anyway, so that a feed behaves the same wherever it is posted
// from and there is one place that knows Discord's content limit and its 429s.
//
// A webhook URL is a bearer credential: anyone holding it can post as that
// channel. Nothing here ever puts one in an error or a log line.
package webhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// maxContent is Discord's limit on a message body. A batch is flushed before
	// it would cross this rather than truncated, so a line is never cut in half.
	maxContent = 2000
	// postTimeout bounds one request. A feed is not worth holding a goroutine on
	// for longer than this.
	postTimeout = 15 * time.Second
	// maxRetries is how many times a 429 is waited out before the message is
	// dropped. A feed is not worth an unbounded retry: the next window, or the
	// next login, will say the same thing again.
	maxRetries = 2
)

// Client is one webhook URL. It is safe for concurrent use.
type Client struct {
	url  string
	http *http.Client
}

// New returns a client for one webhook URL.
func New(url string) *Client {
	return &Client{url: strings.TrimRight(url, "/"), http: &http.Client{Timeout: postTimeout}}
}

// Post sends a message and returns the id Discord filed it under, which is what
// makes it editable later. The id is what a feed that keeps one message up to
// date needs; a feed that only appends can ignore it.
func (c *Client) Post(content string) (string, error) {
	b, err := c.do(http.MethodPost, c.url+"?wait=true", content)
	if err != nil {
		return "", err
	}
	var msg struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &msg); err != nil {
		return "", fmt.Errorf("webhook: reply: %w", err)
	}
	return msg.ID, nil
}

// Edit replaces the content of a message this webhook posted. A message that is
// gone — the channel was cleared, or somebody deleted it — comes back as
// ErrGone, which is the caller's cue to post a new one.
func (c *Client) Edit(id, content string) error {
	_, err := c.do(http.MethodPatch, c.url+"/messages/"+id, content)
	return err
}

// ErrGone reports a message that is no longer there to edit.
var ErrGone = errors.New("webhook: message is gone")

type payload struct {
	Content         string          `json:"content"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

// allowedMentions renders a mention without notifying anyone. A feed is a
// record, and an IGN or a tag that happens to look like a mention must not ping
// a channel every time that player logs in.
type allowedMentions struct {
	Parse []string `json:"parse"`
}

func (c *Client) do(method, url, content string) ([]byte, error) {
	if content == "" {
		return nil, errors.New("webhook: empty message")
	}
	content = clip(content)
	body, err := json.Marshal(payload{Content: content, AllowedMentions: allowedMentions{Parse: []string{}}})
	if err != nil {
		return nil, err
	}

	for try := 0; ; try++ {
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			// The URL carries the token, so it never goes in the error. So does
			// the wrapped *url.Error, which is why this is not %w.
			return nil, fmt.Errorf("webhook: %s: %v", method, redact(err))
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusTooManyRequests && try < maxRetries:
			time.Sleep(retryAfter(resp, b))
		case resp.StatusCode == http.StatusNotFound && method == http.MethodPatch:
			return nil, ErrGone
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return b, nil
		default:
			return nil, fmt.Errorf("webhook: %s", resp.Status)
		}
	}
}

// clip bounds a message at Discord's limit without cutting a rune in half. The
// ellipsis has to fit inside the limit too, which is why this is not one slice.
// The limit is Discord's in characters and this counts bytes, so a message of
// multi-byte characters is cut early rather than late — the safe direction.
func clip(s string) string {
	if len(s) <= maxContent {
		return s
	}
	const ellipsis = "…"
	cut := maxContent - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}

// retryAfter reads how long Discord wants us to wait. The body's figure is
// seconds as a float and is the more precise of the two; the header is the
// fallback, and a second is the fallback for that.
func retryAfter(resp *http.Response, body []byte) time.Duration {
	var lim struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &lim) == nil && lim.RetryAfter > 0 {
		return time.Duration(lim.RetryAfter * float64(time.Second))
	}
	if h := resp.Header.Get("Retry-After"); h != "" {
		var s float64
		if _, err := fmt.Sscanf(h, "%g", &s); err == nil && s > 0 {
			return time.Duration(s * float64(time.Second))
		}
	}
	return time.Second
}

// redact keeps a webhook URL out of an error. net/http puts the whole URL in
// *url.Error, and that URL is the credential.
func redact(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) {
		return ue.Unwrap().Error()
	}
	return err.Error()
}

// Queue is a fire-and-forget feed. Send never blocks and never fails: a caller
// on the relay path must not wait on Discord and must not care whether Discord
// is reachable. Lines are coalesced into one message per flush, which is what
// keeps a reconnect storm inside Discord's rate limit and what makes it
// readable at the same time.
type Queue struct {
	c     *Client
	in    chan string
	done  chan struct{}
	drops atomic.Int64
	once  sync.Once
}

// NewQueue starts a queue. depth is how many lines may wait; past it the oldest
// are dropped and the count is reported in the next message, because a feed that
// blocks a login is worse than a feed with a hole in it. flush is how long lines
// are gathered before one message goes out.
func NewQueue(url string, depth int, flush time.Duration) *Queue {
	q := &Queue{c: New(url), in: make(chan string, depth), done: make(chan struct{})}
	go q.run(flush)
	return q
}

// Send offers one line. It returns immediately whatever the state of the queue.
func (q *Queue) Send(line string) {
	if line == "" {
		return
	}
	select {
	case q.in <- line:
	default:
		q.drops.Add(1)
	}
}

// Close flushes what is queued and stops the sender.
func (q *Queue) Close() {
	q.once.Do(func() { close(q.in) })
	<-q.done
}

func (q *Queue) run(flush time.Duration) {
	defer close(q.done)
	t := time.NewTicker(flush)
	defer t.Stop()

	var buf []string
	n := 0
	send := func() {
		if len(buf) == 0 {
			return
		}
		if d := q.drops.Swap(0); d > 0 {
			buf = append(buf, fmt.Sprintf("_...and %d more that did not fit_", d))
		}
		if _, err := q.c.Post(strings.Join(buf, "\n")); err != nil {
			log.Printf("%v", err)
		}
		buf, n = buf[:0], 0
	}
	for {
		select {
		case line, ok := <-q.in:
			if !ok {
				send()
				return
			}
			if n+len(line)+1 > maxContent {
				send()
			}
			buf = append(buf, line)
			n += len(line) + 1
		case <-t.C:
			send()
		}
	}
}
