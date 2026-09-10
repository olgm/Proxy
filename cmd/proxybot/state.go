package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// statePath is the one thing the bot remembers between restarts: the ids of the
// messages it keeps up to date, so it edits them rather than posting a new set
// every time it starts. It is deliberately not a second source of truth for
// anything — losing it costs a duplicate message and nothing else.
const statePath = "/var/lib/proxybot/feeds.json"

type state struct {
	OnlineMessage string `json:"online_message,omitempty"`
	// BoardMessage is the status card, which lives at the foot of the status
	// channel.
	BoardMessage string `json:"board_message,omitempty"`
	// Issues are the faults that have been announced and not yet recovered,
	// keyed by node. Keeping them is what lets a recovery go back and quieten
	// the line that raised the alarm, across a restart of this process as well
	// as within one.
	Issues map[string]issue `json:"issues,omitempty"`
}

type issue struct {
	// Message is the id of the line that announced the fault.
	Message string `json:"message"`
	// Cond is what that line said. A bot that has just started compares this
	// against what it now sees, so a fault that is still going is not announced
	// a second time.
	Cond string `json:"cond"`
	// Since is when it was announced, which is what "unreachable for 3m" counts
	// from.
	Since time.Time `json:"since"`
}

// store is the state file, and the lock over it. Two feeds keep ids in the same
// file and each writes the whole of it, so every write has to be a
// read-modify-write under one lock: without it the roster's save would drop the
// card's id every twenty seconds, and the card would pile up a new message each
// time.
type store struct {
	path string
	mu   sync.Mutex
	s    state
}

func openStore(path string) *store {
	st := &store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("state: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return st // no file yet is the normal first run
	}
	if err := json.Unmarshal(b, &st.s); err != nil {
		log.Printf("state: %s: %v; starting a new set of messages", path, err)
	}
	return st
}

// get is a copy of the current state. The Issues map is copied too, so a caller
// holding it cannot change what is on disk without saying so.
func (st *store) get() state {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := st.s
	out.Issues = make(map[string]issue, len(st.s.Issues))
	for k, v := range st.s.Issues {
		out.Issues[k] = v
	}
	return out
}

// update applies f to the state and writes it out.
func (st *store) update(f func(*state)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.s.Issues == nil {
		st.s.Issues = map[string]issue{}
	}
	f(&st.s)
	st.save()
}

// save writes through a temporary file, so a crash mid-write cannot leave a
// truncated id that would be read back as "no message". Called with the lock.
func (st *store) save() {
	b, err := json.Marshal(st.s)
	if err != nil {
		return
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("state: %s: %v", st.path, err)
		return
	}
	if err := os.Rename(tmp, st.path); err != nil {
		log.Printf("state: %s: %v", st.path, err)
	}
}
