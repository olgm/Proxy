package probe

import (
	"fmt"
	"os"
	"time"

	"github.com/olgm/proxy/internal/webhook"
)

// EnvProbeWebhook names the environment variable holding the webhook URL for
// the probe feed. proxyctl writes it into the node's env file from whatever
// variable topology.json named; probed reads it here. Unset means the feed is
// off, which is the normal state.
const EnvProbeWebhook = "PROBED_PROBE_WEBHOOK"

// FeedWindows picks which windows reach the channel: the ones asked for, or the
// longest configured when none were. The longest is the default because it is
// the only one whose p99 has enough samples behind it to mean anything, and
// because without a filter a node originating four classes posts eight messages
// a minute, which nobody reads.
//
// probed and proxyctl both need this answer — one to obey it, one to print it —
// so it lives here rather than in each of them.
func FeedWindows(configured, want []string) []string {
	if len(want) > 0 {
		return want
	}
	longest, d := "", time.Duration(0)
	for _, w := range configured {
		p, err := time.ParseDuration(w)
		if err == nil && p > d {
			longest, d = w, p
		}
	}
	if longest == "" {
		return nil
	}
	return []string{longest}
}

// feed posts one line per window per class to a Discord webhook. It is nil
// unless the node was given a URL, and every method tolerates that.
//
// Only an originator has anything to say: a node that merely answers holds no
// measurement of its own. That falls out of where this is called from, which is
// the same place the dataset is written.
type feed struct {
	q       *webhook.Queue
	node    string
	windows map[time.Duration]bool
}

func newFeed(cfg Config) (*feed, error) {
	url := os.Getenv(EnvProbeWebhook)
	if url == "" {
		return nil, nil
	}
	want := map[time.Duration]bool{}
	for _, w := range FeedWindows(cfg.Windows, cfg.FeedWindows) {
		d, err := time.ParseDuration(w)
		if err != nil {
			return nil, fmt.Errorf("feed_windows: %q: %w", w, err)
		}
		want[d] = true
	}
	if len(want) == 0 {
		return nil, nil
	}
	// Every class on a node flushes a closed window in the same tick, so a few
	// seconds of gathering turns one window into one message rather than one
	// message per class.
	return &feed{q: webhook.NewQueue(url, 64, 3*time.Second), node: cfg.Name, windows: want}, nil
}

func (f *feed) post(r *Report) {
	if f == nil || !f.windows[r.Window] || r.Sent == 0 {
		return
	}
	fwd, rev, rt := r.Loss()
	line := fmt.Sprintf("**%s** `%s` %s ×%d · %s · p50 %.1fms p90 %.1fms p99 %.1fms mdev %.2f · loss %.2f%%",
		f.node, r.Class, r.Kind, r.Dup, dur(r.Window), r.P50, r.P90, r.P99, r.Mdev, rt)
	// The split is absent on the first window of a series and cannot be
	// anything else: the count that arrived is the difference of two counters,
	// and the earlier one comes from the window before.
	if fwd >= 0 && rev >= 0 {
		line += fmt.Sprintf(" (out %.2f back %.2f)", fwd, rev)
	}
	f.q.Send(line + fmt.Sprintf(" · n %d", r.N))
}

func (f *feed) Close() {
	if f != nil {
		f.q.Close()
	}
}
