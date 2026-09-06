package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/control"
	"github.com/olgm/proxy/internal/webhook"
)

func live(name string, mins int) control.Live {
	return control.Live{Name: name, UUID: "u-" + name, Since: time.Now().Add(-time.Duration(mins) * time.Minute)}
}

// The order is the topology's, not the chain's and not the alphabet's.
func TestRosterGroupsByNodeInTheOrderAsked(t *testing.T) {
	got := render([]nodeLive{
		{node: "ch", live: []control.Live{live("Dinnerbone", 3)}},
		{node: "hk", live: []control.Live{live("Notch", 5)}},
		{node: "au", live: []control.Live{live("jeb_", 90)}},
	}, []string{"au", "hk", "ty", "ch"})

	au, hk, ch := strings.Index(got, "**au**"), strings.Index(got, "**hk**"), strings.Index(got, "**ch**")
	if !(au >= 0 && au < hk && hk < ch) {
		t.Fatalf("nodes are not in the order asked for:\n%s", got)
	}
	if !strings.Contains(got, "**Online — 3**") {
		t.Errorf("wrong total:\n%s", got)
	}
	if !strings.Contains(got, "1h30m") {
		t.Errorf("a long session should read in hours:\n%s", got)
	}
}

// An entry not named in the order still appears, at the end. Silently dropping
// a new entry would be the worse failure.
func TestRosterKeepsNodesTheOrderDidNotName(t *testing.T) {
	got := render([]nodeLive{
		{node: "new", live: []control.Live{live("Alex", 1)}},
		{node: "au", live: []control.Live{live("jeb_", 1)}},
	}, []string{"au"})

	if strings.Index(got, "**au**") > strings.Index(got, "**new**") {
		t.Fatalf("a named node did not come first:\n%s", got)
	}
	if !strings.Contains(got, "**new**") {
		t.Fatalf("an unnamed node was dropped:\n%s", got)
	}
}

// "Nobody is online there" and "I could not reach it" must not read the same.
func TestRosterSaysWhenANodeCouldNotBeAsked(t *testing.T) {
	got := render([]nodeLive{
		{node: "au", live: nil},
		{node: "hk", err: io.ErrUnexpectedEOF},
	}, []string{"au", "hk"})

	if !strings.Contains(got, "**hk** unreachable") {
		t.Fatalf("an unreachable node is not reported:\n%s", got)
	}
	if strings.Contains(got, "**au**") {
		t.Errorf("a quiet node should not take a line:\n%s", got)
	}
	if !strings.Contains(got, "**Online — 0**") {
		t.Errorf("an unreachable node must not be counted as players:\n%s", got)
	}
}

func TestRosterWithNobodyOnline(t *testing.T) {
	got := render([]nodeLive{{node: "au"}}, []string{"au"})
	if !strings.Contains(got, "nobody") || !strings.Contains(got, "**Online — 0**") {
		t.Fatalf("empty roster reads wrong:\n%s", got)
	}
}

// An IGN may contain an underscore, which Discord reads as markup.
func TestRosterKeepsNamesOutOfMarkup(t *testing.T) {
	got := render([]nodeLive{{node: "au", live: []control.Live{live("_x_", 1)}}}, nil)
	if !strings.Contains(got, "`_x_`") {
		t.Fatalf("name is not in an inline code span:\n%s", got)
	}
}

type editCapture struct {
	mu      sync.Mutex
	posts   int
	edits   int
	gone    bool
	bodies  []string
	editIDs []string
	url     string
}

func newEditCapture(t *testing.T) *editCapture {
	t.Helper()
	c := &editCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p struct {
			Content string `json:"content"`
		}
		json.Unmarshal(b, &p)

		c.mu.Lock()
		defer c.mu.Unlock()
		c.bodies = append(c.bodies, p.Content)
		if r.Method == http.MethodPatch {
			c.edits++
			c.editIDs = append(c.editIDs, filepath.Base(r.URL.Path))
			if c.gone {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{}`))
				return
			}
			w.Write([]byte(`{"id":"777"}`))
			return
		}
		c.posts++
		w.Write([]byte(`{"id":"777"}`))
	}))
	t.Cleanup(srv.Close)
	c.url = srv.URL
	return c
}

func (c *editCapture) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.posts, c.edits
}

func testRoster(t *testing.T, url string) *roster {
	t.Helper()
	return &roster{c: webhook.New(url), path: filepath.Join(t.TempDir(), "feeds.json")}
}

// The first draw posts and remembers the id; the next one edits that message
// rather than adding another. An unchanged roster is not re-edited at all.
func TestRosterEditsRatherThanReposting(t *testing.T) {
	c := newEditCapture(t)
	r := testRoster(t, c.url)

	if err := r.publish("first"); err != nil {
		t.Fatal(err)
	}
	if err := r.publish("second"); err != nil {
		t.Fatal(err)
	}
	posts, edits := c.counts()
	if posts != 1 || edits != 1 {
		t.Fatalf("posts=%d edits=%d, want 1 and 1", posts, edits)
	}
	if r.id != "777" {
		t.Fatalf("message id = %q", r.id)
	}
	// And it survives a restart, which is the only reason the file exists.
	if got := loadState(r.path).OnlineMessage; got != "777" {
		t.Fatalf("state file holds %q", got)
	}
}

// Somebody cleared the channel. Editing a message that is gone is not a failure
// to report and stop on: it is the cue to post a new one.
func TestRosterRepostsWhenTheMessageIsGone(t *testing.T) {
	c := newEditCapture(t)
	r := testRoster(t, c.url)
	r.id = "old"
	c.gone = true

	if err := r.publish("body"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	posts, edits := c.counts()
	if posts != 1 || edits != 1 {
		t.Fatalf("posts=%d edits=%d, want one failed edit then one post", posts, edits)
	}
	if r.id != "777" {
		t.Fatalf("did not take the new id: %q", r.id)
	}
}

// A roster nobody configured is nil, and a nil roster is simply never drawn.
func TestNoRosterIsNoRoster(t *testing.T) {
	t.Setenv("PROXYBOT_ONLINE_WEBHOOK", "")
	if r := newRoster(nil, nil, filepath.Join(t.TempDir(), "s.json")); r != nil {
		t.Fatal("an unset URL produced a roster")
	}
}
