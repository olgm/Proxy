// Package whitelist gates logins on the identity a client claims in Login Start.
//
// That claim cannot be verified here. proxyd never terminates Minecraft's
// encryption, so it can never ask Mojang whether a UUID is really that player's —
// see agents/hypixel-protocol.md §4. Everything this package allows, it allows on
// the client's word. It keeps uninvited players off the chain; it is not an
// authentication boundary, and it is not a rate limit.
package whitelist

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mojang is the out-of-band source for the name/UUID binding. Nothing on our wire
// carries it, so without this a name recorded months ago is honoured forever — long
// after its owner released it and someone else claimed it.
type Mojang interface {
	// NameFor returns the name a UUID answers to today.
	NameFor(uuid string) (string, bool)
	// UUIDFor returns the UUID that owns name right now. It reports false when its
	// own rate limiters refuse the lookup, so a miss must never be read as "no".
	UUIDFor(name, ip string) (string, bool)
}

// List is a file-backed set of players, in `ign:uuid` lines. Comments (`#`) and
// blank lines are preserved so an operator can annotate the file.
//
// The UUID column is the identity; the name column is only a cache of what that
// UUID is called today, kept fresh against Mojang.
type List struct {
	path   string
	mojang Mojang

	mu     sync.Mutex
	lines  []string          // the file verbatim, so a rewrite keeps its comments
	byUUID map[string]*entry // normalised uuid -> entry
	byName map[string]*entry // lowercased ign -> entry
	mtime  time.Time
	size   int64
	mode   os.FileMode
}

type entry struct {
	name string
	uuid string // as written in the file, dashed or bare
	line int    // index into List.lines
}

// Open loads the list. A list that cannot be read or parsed is an error: an ingress
// configured to be gated must not come up ungated. A nil Mojang leaves the list
// matching on what is written in the file and nothing else.
func Open(path string, m Mojang) (*List, error) {
	l := &List{path: path, mojang: m}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

// Len reports how many players are on the list.
func (l *List) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byUUID)
}

// Check reports whether a login from ip may proceed.
//
// Given a UUID it matches on the UUID, and rewrites the stored name when the player
// has renamed. A UUID that is not listed is refused outright: the client named an
// identity, and it is not one of ours.
//
// Given no UUID — every client before 1.19 — it matches the name, and on a miss asks
// Mojang who owns that name now. That second step is what stops a released name from
// working: the answer is checked against the UUID column, not the name column, so a
// stranger who claimed a name we once wrote down resolves to their own UUID and is
// refused.
func (l *List) Check(name, uuid, ip string) bool {
	matched, listed := l.checkLocal(name, uuid)
	if listed {
		return matched
	}
	// Not in the file, and no UUID to judge it by. Everything past here is a network
	// call, so it happens with the lock released.
	return l.claimedBy(name, ip)
}

// checkLocal answers from the file alone. listed reports whether it could: when it
// is false the caller has to ask Mojang, and matched is meaningless.
func (l *List) checkLocal(name, uuid string) (matched, listed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reload()

	if uuid != "" {
		e, ok := l.byUUID[normalize(uuid)]
		if !ok {
			return false, true // named an identity, and it is not one of ours
		}
		if !strings.EqualFold(e.name, name) {
			l.rename(e, name)
		}
		return true, true
	}

	if _, ok := l.byName[strings.ToLower(name)]; ok {
		// Trusted without asking Mojang. Refresh keeps this column no more than a
		// day stale, and a released name cannot be re-registered for far longer
		// than that, so a name still in the file is still its owner's.
		return true, true
	}
	return false, false
}

// claimedBy handles a name that is not in the file at all. Usually a stranger, but
// also the shape of a listed player who renamed and logged straight back in on a
// client too old to send a UUID. Ask Mojang who owns the name, and let them in only
// if that UUID is listed — then record the new name, so it never has to be asked
// again.
//
// The lookup runs unlocked. It can take 650 ms, and up to 5 s if Mojang is
// unreachable; holding the list across it would stall every other login on the node
// behind one stranger's miss.
func (l *List) claimedBy(name, ip string) bool {
	if l.mojang == nil {
		return false
	}
	uuid, ok := l.mojang.UUIDFor(name, ip)
	if !ok {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// The file may have been edited or refreshed while we were asking.
	e, ok := l.byUUID[normalize(uuid)]
	if !ok {
		return false
	}
	l.rename(e, name)
	return true
}

// apply moves an entry to a new name in memory and in the pending file text. The
// caller reindexes and saves; the refresh does many of these before either.
func (l *List) apply(e *entry, name string) {
	log.Printf("whitelist: %s is now %s (%s)", e.name, name, e.uuid)
	e.name = name
	l.lines[e.line] = name + ":" + e.uuid
}

// rename applies one change and writes the file back. Every rename is logged: it is
// the only path by which a connecting client can change state on the node.
func (l *List) rename(e *entry, name string) {
	l.apply(e, name)
	l.reindex()
	if err := l.save(); err != nil {
		log.Printf("whitelist: save %s: %v", l.path, err)
	}
}

// reindex rebuilds the name index from the entries. A name two entries somehow share
// is left out of it: the name fallback exists to identify one player, and an
// ambiguous name identifies nobody. UUID matching is unaffected.
func (l *List) reindex() {
	l.byName = make(map[string]*entry, len(l.byUUID))
	var dup []string
	for _, e := range l.byUUID {
		k := strings.ToLower(e.name)
		if _, seen := l.byName[k]; seen {
			dup = append(dup, k)
			continue
		}
		l.byName[k] = e
	}
	for _, k := range dup {
		log.Printf("whitelist: %q is on two entries; name matching disabled for it", k)
		delete(l.byName, k)
	}
}

// reload re-reads the file when it has changed on disk, so the list can be edited
// without a restart — restarting proxyd would drop every player mid-session. A file
// that fails to parse, which a half-finished edit will, leaves the last good list in
// place rather than opening or closing the gate on a typo.
func (l *List) reload() {
	fi, err := os.Stat(l.path)
	if err != nil {
		log.Printf("whitelist: stat %s: %v (keeping loaded list)", l.path, err)
		return
	}
	if fi.ModTime().Equal(l.mtime) && fi.Size() == l.size {
		return
	}
	if err := l.load(); err != nil {
		log.Printf("whitelist: reload %s: %v (keeping loaded list)", l.path, err)
	}
}

func (l *List) load() error {
	b, err := os.ReadFile(l.path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(l.path)
	if err != nil {
		return err
	}

	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	byUUID := map[string]*entry{}
	for i, raw := range lines {
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		name, uuid, ok := strings.Cut(s, ":")
		name, uuid = strings.TrimSpace(name), strings.TrimSpace(uuid)
		if !ok || name == "" {
			return fmt.Errorf("%s:%d: want ign:uuid, got %q", l.path, i+1, s)
		}
		key := normalize(uuid)
		if len(key) != 32 || strings.TrimLeft(key, "0123456789abcdef") != "" {
			return fmt.Errorf("%s:%d: %q is not a uuid", l.path, i+1, uuid)
		}
		if _, dup := byUUID[key]; dup {
			return fmt.Errorf("%s:%d: %s is listed twice", l.path, i+1, uuid)
		}
		byUUID[key] = &entry{name: name, uuid: uuid, line: i}
	}

	l.lines, l.byUUID = lines, byUUID
	l.reindex()
	l.mtime, l.size, l.mode = fi.ModTime(), fi.Size(), fi.Mode().Perm()
	return nil
}

// save rewrites the file atomically. A torn write would be read straight back by the
// next reload as a truncated list, locking out everyone below the tear.
func (l *List) save() error {
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".whitelist-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.WriteString(strings.Join(l.lines, "\n") + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(l.mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), l.path); err != nil {
		return err
	}
	// Adopt the new mtime, or the next Check re-reads our own write.
	if fi, err := os.Stat(l.path); err == nil {
		l.mtime, l.size = fi.ModTime(), fi.Size()
	}
	return nil
}

// normalize reduces both spellings Mojang uses — dashed and bare hex — to one
// comparable form.
func normalize(uuid string) string {
	return strings.ToLower(strings.ReplaceAll(uuid, "-", ""))
}
