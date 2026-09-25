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
	"mime/multipart"
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

// Message is one message: what it says, who it may notify, and the one image it
// may carry. The zero value notifies nobody and carries nothing, which is what
// every feed but the status card wants.
type Message struct {
	Content string
	// Ping are role ids this message is allowed to notify. Everything else stays
	// suppressed, including @everyone and anything inside an IGN that happens to
	// look like a mention. A feed that pings is opt-in, one role at a time.
	Ping []string
	// Image, when set, is the message's only attachment. Updating a message that
	// has one replaces it: Discord keeps the attachments a PATCH lists and drops
	// the rest, and this lists only the new one.
	Image *Image
}

// Image is a file to attach. Name is what Discord shows and what the CDN URL
// ends in; it is not a path and nothing here opens a file.
type Image struct {
	Name string
	PNG  []byte
}

// Post sends a message and returns the id Discord filed it under, which is what
// makes it editable later. The id is what a feed that keeps one message up to
// date needs; a feed that only appends can ignore it.
func (c *Client) Post(content string) (string, error) {
	return c.Send(Message{Content: content})
}

// Send posts a Message and returns its id.
func (c *Client) Send(m Message) (string, error) {
	b, err := c.do(http.MethodPost, c.url+"?wait=true", m)
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
	return c.Update(id, Message{Content: content})
}

// Update replaces a message with m, image and all. Editing does not notify
// anyone however Ping is set: Discord pings at post time, so a line that has
// already been read cannot be made to ring again by rewriting it.
func (c *Client) Update(id string, m Message) error {
	_, err := c.do(http.MethodPatch, c.url+"/messages/"+id, m)
	return err
}

// Delete removes a message this webhook posted. A message that is already gone
// is not an error: the caller wanted it gone and it is.
func (c *Client) Delete(id string) error {
	_, err := c.do(http.MethodDelete, c.url+"/messages/"+id, Message{})
	if err == ErrGone {
		return nil
	}
	return err
}

// ErrGone reports a message that is no longer there to edit.
var ErrGone = errors.New("webhook: message is gone")

type payload struct {
	Content         string          `json:"content"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
	// Attachments is what the message should have after this request. A PATCH
	// keeps what it lists and drops what it does not, so listing only the upload
	// is how the previous card stops being part of the message.
	Attachments []attachment `json:"attachments,omitempty"`
}

type attachment struct {
	ID       int    `json:"id"`
	Filename string `json:"filename"`
}

// allowedMentions renders a mention without notifying anyone, except the roles
// named. A feed is a record, and an IGN or a tag that happens to look like a
// mention must not ping a channel every time that player logs in; a node going
// down is the one thing worth waking somebody for, and only the role configured
// for it.
type allowedMentions struct {
	Parse []string `json:"parse"`
	Roles []string `json:"roles,omitempty"`
}

func (c *Client) do(method, url string, m Message) ([]byte, error) {
	body, ctype, err := encode(method, m)
	if err != nil {
		return nil, err
	}

	for try := 0; ; try++ {
		req, err := http.NewRequest(method, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
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
		case resp.StatusCode == http.StatusNotFound && (method == http.MethodPatch || method == http.MethodDelete):
			return nil, ErrGone
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return b, nil
		default:
			return nil, fmt.Errorf("webhook: %s", resp.Status)
		}
	}
}

// encode builds the request body. A message with no image is plain JSON, the
// same bytes this package always sent; one with an image has to be multipart,
// because Discord takes an upload and its metadata in a single request. A DELETE
// carries no body at all.
func encode(method string, m Message) ([]byte, string, error) {
	if method == http.MethodDelete {
		return nil, "", nil
	}
	if m.Content == "" && m.Image == nil {
		return nil, "", errors.New("webhook: empty message")
	}
	p := payload{
		Content:         clip(m.Content),
		AllowedMentions: allowedMentions{Parse: []string{}, Roles: m.Ping},
	}
	if m.Image != nil {
		p.Attachments = []attachment{{ID: 0, Filename: m.Image.Name}}
	}
	j, err := json.Marshal(p)
	if err != nil {
		return nil, "", err
	}
	if m.Image == nil {
		return j, "application/json", nil
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("payload_json", string(j)); err != nil {
		return nil, "", err
	}
	// The part name has to be files[0] and its index has to match the id in
	// Attachments above: that pairing is how Discord knows which upload the
	// metadata describes.
	part, err := w.CreateFormFile("files[0]", m.Image.Name)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(m.Image.PNG); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
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
	c    *Client
	in   chan string
	stop chan struct{}
	done chan struct{}

	// closed is guarded rather than signalled by closing in, because a node
	// shutting down closes the feed while sessions are still ending: a send on
	// a closed channel would take the process down with it, and the whole point
	// of this queue is that a feed cannot hurt the thing it reports on.
	mu     sync.RWMutex
	closed bool
	// detached says the queue is stopping to be carried on elsewhere: what it
	// has not posted is handed back in left instead of being posted.
	detached bool
	left     []string

	drops atomic.Int64
	once  sync.Once
}

// NewQueue starts a queue. depth is how many lines may wait; past it a new line
// is dropped and counted, and the count is reported, because a feed that blocks
// a login is worse than a feed with a hole in it. flush is how long lines are
// gathered before one message goes out — at most, since a queue that is filling
// up posts early rather than waiting for the timer and losing the overflow.
func NewQueue(url string, depth int, flush time.Duration) *Queue {
	q := &Queue{
		c: New(url), in: make(chan string, depth),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go q.run(flush)
	return q
}

// Send offers one line. It returns immediately whatever the state of the queue,
// including after Close: a session ending during shutdown must not block and
// must not panic.
func (q *Queue) Send(line string) {
	if line == "" {
		return
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return
	}
	select {
	case q.in <- line:
	default:
		q.drops.Add(1)
	}
}

// Close stops accepting lines, flushes what is queued, and waits for the sender
// to finish. It may be called more than once.
func (q *Queue) Close() {
	q.once.Do(func() {
		q.mu.Lock()
		q.closed = true
		q.mu.Unlock()
		close(q.stop)
	})
	<-q.done
}

// Detach stops the queue without posting and returns, in order, every line it
// had not posted yet, for a process that carries on from this one to Send. It
// waits at most d: a post already on its way to Discord is left to finish on its
// own, and what was gathered behind it goes with the process. Nil if nothing was
// waiting, or if the wait ran out.
func (q *Queue) Detach(d time.Duration) []string {
	q.once.Do(func() {
		q.mu.Lock()
		q.closed, q.detached = true, true
		q.mu.Unlock()
		close(q.stop)
	})
	select {
	case <-q.done:
		return q.left
	case <-time.After(d):
		return nil
	}
}

func (q *Queue) run(flush time.Duration) {
	defer close(q.done)
	t := time.NewTicker(flush)
	defer t.Stop()

	var buf []string
	n := 0
	send := func() {
		// Before the empty check, not after it. A drop is most likely when the
		// sender was stuck on a slow post and everything queued behind it was
		// turned away, which is exactly the case that leaves nothing else to
		// carry the note — and the one where nobody must be left thinking the
		// feed was simply quiet.
		if d := q.drops.Swap(0); d > 0 {
			buf = append(buf, fmt.Sprintf("_...and %d more that did not fit_", d))
		}
		if len(buf) == 0 {
			return
		}
		if _, err := q.c.Post(strings.Join(buf, "\n")); err != nil {
			log.Printf("%v", err)
		}
		buf, n = buf[:0], 0
	}
	take := func(line string) {
		if n+len(line)+1 > maxContent {
			send()
		}
		buf = append(buf, line)
		n += len(line) + 1
	}
	for {
		select {
		case line := <-q.in:
			take(line)
			// Post early while the queue is filling instead of sitting on the
			// timer until it overflows. A line dropped for want of room is a
			// login or a logout nobody will ever see reported, and the room is
			// only ever short because this loop was slow to empty it.
			if len(q.in) >= cap(q.in)/2 {
				send()
			}
		case <-t.C:
			send()
		case <-q.stop:
			q.mu.RLock()
			detached := q.detached
			q.mu.RUnlock()
			if detached {
				for {
					select {
					case line := <-q.in:
						buf = append(buf, line)
						continue
					default:
					}
					if d := q.drops.Swap(0); d > 0 {
						buf = append(buf, fmt.Sprintf("_...and %d more that did not fit_", d))
					}
					q.left = buf
					return
				}
			}
			// Send is shut out by now, so what is in the channel is all there
			// will ever be. Drain it rather than dropping a logout posted a
			// moment before the node went down.
			for {
				select {
				case line := <-q.in:
					take(line)
				default:
					send()
					return
				}
			}
		}
	}
}
