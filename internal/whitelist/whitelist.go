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

// List is a file-backed set of players, in `ign:uuid` lines. Comments (`#`) and
// blank lines are preserved so an operator can annotate the file.
type List struct {
	path string

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
// configured to be gated must not come up ungated.
func Open(path string) (*List, error) {
	l := &List{path: path}
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

// Check reports whether a login may proceed.
//
// Given a UUID it matches on the UUID, and rewrites the stored IGN when the player
// has renamed since the entry was written. Given none — every client before 1.19 —
// it falls back to the IGN, which is all those clients send.
func (l *List) Check(name, uuid string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reload()

	if uuid == "" {
		_, ok := l.byName[strings.ToLower(name)]
		return ok
	}
	e, ok := l.byUUID[normalize(uuid)]
	if !ok {
		return false
	}
	if !strings.EqualFold(e.name, name) {
		l.rename(e, name)
	}
	return true
}

// rename updates the IGN column of an entry whose UUID matched, and writes the file
// back. The new name is the client's unverified claim, exactly like the UUID that
// selected the entry, so every rename is logged: this is the only path by which a
// connecting client can change state on the node.
func (l *List) rename(e *entry, name string) {
	log.Printf("whitelist: %s is now %s (%s)", e.name, name, e.uuid)
	delete(l.byName, strings.ToLower(e.name))
	e.name = name
	l.byName[strings.ToLower(name)] = e
	l.lines[e.line] = name + ":" + e.uuid
	if err := l.save(); err != nil {
		log.Printf("whitelist: save %s: %v", l.path, err)
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
	byName := map[string]*entry{}
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
		e := &entry{name: name, uuid: uuid, line: i}
		byUUID[key] = e
		byName[strings.ToLower(name)] = e
	}

	l.lines, l.byUUID, l.byName = lines, byUUID, byName
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
