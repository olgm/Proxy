package whitelist

import (
	"log"
	"os"
	"strings"
	"time"
)

const (
	// RefreshInterval is how often every entry's name is re-read from Mojang. A
	// released name cannot be claimed by anyone else for far longer than this, so a
	// day leaves an enormous margin: the name column is never stale enough for a
	// recycled name to be honoured.
	RefreshInterval = 24 * time.Hour

	// settle delays the first refresh after start. proxyd restarts on failure every
	// few seconds, and a crash loop must not turn into a burst of Mojang traffic.
	settle = 30 * time.Second
)

// stampSuffix names the sidecar recording the last refresh. It is kept beside the
// list rather than inside it so the operator's file stays purely their content, and
// on disk rather than in memory so a restart does not re-trigger a refresh.
const stampSuffix = ".refreshed"

// StartRefresh runs the name refresh on a schedule for the life of the process. It
// is what keeps a name-matched login meaningful; without it the name column only
// records what players were called on the day they were added.
func (l *List) StartRefresh() {
	if l.mojang == nil {
		return
	}
	go func() {
		for {
			time.Sleep(l.untilNextRefresh())
			l.Refresh()
		}
	}()
}

// untilNextRefresh is the delay before the next run, carrying over what a previous
// process already did.
func (l *List) untilNextRefresh() time.Duration {
	last, err := l.readStamp()
	if err != nil {
		return settle
	}
	if d := time.Until(last.Add(RefreshInterval)); d > settle {
		return d
	}
	return settle
}

// Refresh re-reads every entry's current name from Mojang and writes back the ones
// that changed.
//
// This is the whole answer to recycled names. The list is keyed by UUID, which never
// changes; the name is a cache of what that UUID is called. Keeping the cache fresh
// means a name that has moved on stops matching here before anyone else can claim
// it.
func (l *List) Refresh() {
	if l.mojang == nil {
		return
	}
	// Snapshot under the lock, then let it go: these are network calls, and holding
	// the lock across them would stall every login for the length of the walk.
	l.mu.Lock()
	l.reload()
	uuids := make([]string, 0, len(l.byUUID))
	for _, e := range l.byUUID {
		uuids = append(uuids, e.uuid)
	}
	l.mu.Unlock()

	renamed := map[string]string{}
	for _, uuid := range uuids {
		name, ok := l.mojang.NameFor(uuid)
		if !ok {
			continue // unreachable or unknown; the recorded name stands
		}
		renamed[normalize(uuid)] = name
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	changed := 0
	for key, name := range renamed {
		e, ok := l.byUUID[key] // the file may have been edited while we walked
		if !ok || e.name == name {
			continue
		}
		l.apply(e, name)
		changed++
	}
	if changed > 0 {
		l.reindex()
		if err := l.save(); err != nil {
			log.Printf("whitelist: save %s: %v", l.path, err)
		}
	}
	l.writeStamp()
	log.Printf("whitelist: refreshed %d entries, %d renamed", len(uuids), changed)
}

func (l *List) readStamp() (time.Time, error) {
	b, err := os.ReadFile(l.path + stampSuffix)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
}

func (l *List) writeStamp() {
	stamp := time.Now().UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(l.path+stampSuffix, []byte(stamp), 0o640); err != nil {
		log.Printf("whitelist: stamp %s: %v", l.path+stampSuffix, err)
	}
}
