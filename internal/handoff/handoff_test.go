package handoff

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeSystemd is the receiving end of $NOTIFY_SOCKET, doing what systemd does
// with the messages proxyd sends: keeping named descriptors, dropping them on
// FDSTOREREMOVE, and closing a barrier's pipe once everything before it is in.
type fakeSystemd struct {
	conn *net.UnixConn

	mu     sync.Mutex
	states []string
	store  map[string]int
	order  []string
}

func startSystemd(t *testing.T) *fakeSystemd {
	t.Helper()
	// Short on purpose: a unix socket path has to fit in about a hundred bytes,
	// and a test's own temp dir on macOS does not leave room.
	dir, err := os.MkdirTemp("", "hs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "notify")
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	t.Setenv("NOTIFY_SOCKET", path)
	t.Setenv("FDSTORE", "64")
	t.Setenv("STATE_DIRECTORY", dir)
	s := &fakeSystemd{conn: c, store: map[string]int{}}
	go s.run()
	return s
}

func (s *fakeSystemd) run() {
	buf, oob := make([]byte, 4096), make([]byte, 1024)
	for {
		n, oobn, _, _, err := s.conn.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		var fds []int
		if msgs, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil {
			for _, m := range msgs {
				if got, err := syscall.ParseUnixRights(&m); err == nil {
					fds = append(fds, got...)
				}
			}
		}
		state := string(buf[:n])
		kv := map[string]string{}
		for _, line := range strings.Split(state, "\n") {
			k, v, _ := strings.Cut(line, "=")
			kv[k] = v
		}
		s.mu.Lock()
		s.states = append(s.states, state)
		switch {
		case kv["BARRIER"] == "1":
			for _, fd := range fds {
				syscall.Close(fd)
			}
		case kv["FDSTORE"] == "1":
			if kv["FDPOLL"] != "0" {
				panic("stored without FDPOLL=0: " + state)
			}
			s.store[kv["FDNAME"]] = fds[0]
			s.order = append(s.order, kv["FDNAME"])
		case kv["FDSTOREREMOVE"] == "1":
			syscall.Close(s.store[kv["FDNAME"]])
			delete(s.store, kv["FDNAME"])
		}
		s.mu.Unlock()
	}
}

// pass hands the store to the next process, as LISTEN_FDS would.
func (s *fakeSystemd) pass(t *testing.T) *Inherited {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	var fds []int
	for _, name := range s.order {
		if fd, ok := s.store[name]; ok {
			nfd, err := syscall.Dup(fd)
			if err != nil {
				t.Fatal(err)
			}
			names, fds = append(names, name), append(fds, nfd)
		}
	}
	in, err := inherit(names, fds)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func (s *fakeSystemd) sawState(want string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.states {
		if st == want {
			return true
		}
	}
	return false
}

// A listening socket put in the store comes out the other side still listening:
// a client that connects in between is accepted by the process that takes it.
func TestStoreCarriesASocketAndTheSnapshot(t *testing.T) {
	sd := startSystemd(t)
	if !Available() {
		t.Fatal("not available under a notify socket with a store")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	if err := Store([]byte(`{"v":1}`), []File{{Name: "l0", File: f}}); err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	f.Close()
	ln.Close()

	// Connecting now, with nobody in either process accepting, still succeeds:
	// the socket is systemd's until someone takes it.
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("the stored listener stopped listening: %v", err)
	}
	defer c.Close()

	in := sd.pass(t)
	if string(in.Snapshot) != `{"v":1}` {
		t.Fatalf("snapshot %q", in.Snapshot)
	}
	lf := in.File("l0")
	if lf == nil {
		t.Fatal("the listener did not come through")
	}
	if in.File("l0") != nil {
		t.Fatal("a file handed over twice")
	}
	ln2, err := net.FileListener(lf)
	lf.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	ln2.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	a, err := ln2.Accept()
	if err != nil {
		t.Fatalf("the connection made in between was lost: %v", err)
	}
	a.Close()

	if got := strings.Join(in.Names(), ","); got != "snapshot,l0" {
		t.Fatalf("names %q", got)
	}
	if err := Forget(in.Names()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		sd.mu.Lock()
		left := len(sd.store)
		sd.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d entries still stored after Forget", left)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The snapshot is unlinked before it is stored: nothing else can find it.
func TestSnapshotLeavesNothingOnDisk(t *testing.T) {
	startSystemd(t)
	if err := Store([]byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(os.Getenv("STATE_DIRECTORY"), "handoff-*"))
	if len(left) != 0 {
		t.Fatalf("snapshot left on disk: %v", left)
	}
}

func TestReadyAndStatus(t *testing.T) {
	sd := startSystemd(t)
	if err := Ready("handoff ready"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !sd.sawState("READY=1\nSTATUS=handoff ready") {
		if time.Now().After(deadline) {
			t.Fatal("systemd never heard READY")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Outside systemd there is nobody to tell and nowhere to store anything.
func TestNothingUnderNoSystemd(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	t.Setenv("FDSTORE", "")
	if Available() {
		t.Fatal("available with no notify socket")
	}
	if err := Ready("x"); err != nil {
		t.Fatalf("ready with nobody to tell: %v", err)
	}
	if err := Store(nil, nil); err == nil {
		t.Fatal("stored with nowhere to store")
	}
}

// Descriptors meant for another process are not ours to take.
func TestTakeIgnoresAnotherProcesssFds(t *testing.T) {
	t.Setenv("LISTEN_PID", "1")
	t.Setenv("LISTEN_FDS", "2")
	t.Setenv("LISTEN_FDNAMES", "snapshot:l0")
	in, err := Take()
	if err != nil || in != nil {
		t.Fatalf("took %v, %v", in, err)
	}
	if os.Getenv("LISTEN_FDS") != "" {
		t.Fatal("LISTEN_FDS left behind")
	}
}
