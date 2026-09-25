package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/olgm/proxy/internal/mc"
	"github.com/olgm/proxy/internal/proxy"
)

// systemd is as much of systemd as a handoff uses: a notify socket that takes
// READY, STATUS, stored descriptors, their removal and a barrier, and a way to
// start the next process with the store as LISTEN_FDS.
type systemd struct {
	t    *testing.T
	dir  string
	sock *net.UnixConn

	mu     sync.Mutex
	states []string
	store  map[string]int
	order  []string
}

func newSystemd(t *testing.T) *systemd {
	t.Helper()
	dir, err := os.MkdirTemp("", "sd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(dir, "notify"), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	sd := &systemd{t: t, dir: dir, sock: c, store: map[string]int{}}
	go sd.run()
	return sd
}

func (sd *systemd) run() {
	buf, oob := make([]byte, 4096), make([]byte, 4096)
	for {
		n, oobn, _, _, err := sd.sock.ReadMsgUnix(buf, oob)
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
		kv := map[string]string{}
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			k, v, _ := strings.Cut(line, "=")
			kv[k] = v
		}
		sd.mu.Lock()
		sd.states = append(sd.states, string(buf[:n]))
		switch {
		case kv["BARRIER"] == "1":
			for _, fd := range fds {
				syscall.Close(fd)
			}
		case kv["FDSTORE"] == "1":
			name := kv["FDNAME"]
			if _, dup := sd.store[name]; !dup {
				sd.order = append(sd.order, name)
			}
			sd.store[name] = fds[0]
		case kv["FDSTOREREMOVE"] == "1":
			if fd, ok := sd.store[kv["FDNAME"]]; ok {
				syscall.Close(fd)
				delete(sd.store, kv["FDNAME"])
			}
		}
		sd.mu.Unlock()
	}
}

// start runs proxyd the way the unit does, handing it whatever is in the store.
// LISTEN_PID has to be the new process's own pid, which only the process knows,
// so a shell sets it and execs: the pid survives the exec.
func (sd *systemd) start(bin, cfg string) *exec.Cmd {
	sd.t.Helper()
	sd.mu.Lock()
	var names []string
	var files []*os.File
	for _, name := range sd.order {
		fd, ok := sd.store[name]
		if !ok {
			continue
		}
		nfd, err := syscall.Dup(fd)
		if err != nil {
			sd.t.Fatal(err)
		}
		names = append(names, name)
		files = append(files, os.NewFile(uintptr(nfd), name))
	}
	sd.mu.Unlock()

	script := `exec "$0" -c "$1"`
	if len(files) > 0 {
		script = `LISTEN_PID=$$ exec "$0" -c "$1"`
	}
	cmd := exec.Command("/bin/sh", "-c", script, bin, cfg)
	cmd.ExtraFiles = files
	cmd.Env = append(os.Environ(),
		"NOTIFY_SOCKET="+sd.sock.LocalAddr().String(),
		"FDSTORE=64",
		"STATE_DIRECTORY="+sd.dir,
		"LISTEN_FDS="+strconv.Itoa(len(files)),
		"LISTEN_FDNAMES="+strings.Join(names, ":"),
	)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		sd.t.Fatal(err)
	}
	for _, f := range files {
		f.Close()
	}
	sd.t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		cmd.Wait()
	})
	return cmd
}

func (sd *systemd) readies() int {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	n := 0
	for _, s := range sd.states {
		if strings.HasPrefix(s, "READY=1\nSTATUS=handoff ready") {
			n++
		}
	}
	return n
}

func (sd *systemd) stored() int {
	sd.mu.Lock()
	defer sd.mu.Unlock()
	return len(sd.store)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// echo is a backend that takes a login and then echoes everything after it.
func echo(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				h, err := mc.ReadHandshake(br)
				if err != nil {
					return
				}
				if _, _, err := mc.ReadLoginStart(br, h.ProtocolVersion); err != nil {
					return
				}
				io.Copy(c, br)
			}()
		}
	}()
	return ln.Addr().String()
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// The whole thing, with the real binary: proxyd tells systemd it is ready, a
// player logs in and plays, SIGUSR2 puts everything in the store and the process
// exits, the next one comes up from the store, tells systemd to forget it, and
// the player never notices.
func TestSIGUSR2HandsAPlayerToTheNextProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("builds proxyd")
	}
	bin := filepath.Join(t.TempDir(), "proxyd")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	addr := freePort(t)
	cfg := &proxy.Config{Name: "ch", Listeners: []proxy.Listener{{Bind: addr, Upstream: echo(t),
		Minecraft: &proxy.Minecraft{RewriteHost: "mc.example.com", RewritePort: 25565}}}}
	b, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	sd := newSystemd(t)
	first := sd.start(bin, cfgPath)
	waitUntil(t, "the first process to be ready", func() bool { return sd.readies() == 1 })

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// 1.8: a Login Start that is only a name.
	hs := (&mc.Handshake{ProtocolVersion: 47, Address: "x", Port: 1, Intent: mc.IntentLogin}).Encode()
	login := []byte{0x07, 0x00, 0x05, 'N', 'o', 't', 'c', 'h'}
	c.Write(append(hs, login...))
	roundTrip := func(msg string) {
		t.Helper()
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(c, got); err != nil || string(got) != msg {
			t.Fatalf("sent %q, got %q: %v", msg, got, err)
		}
	}
	roundTrip("before")

	first.Process.Signal(syscall.SIGUSR2)
	if err := first.Wait(); err != nil {
		t.Fatalf("the first process did not exit cleanly after handing off: %v", err)
	}
	if n := sd.stored(); n < 3 {
		t.Fatalf("%d descriptor(s) stored, want the snapshot, the listener and the player", n)
	}
	// Between processes: the player's bytes wait in the kernel.
	c.Write([]byte("between"))

	sd.start(bin, cfgPath)
	waitUntil(t, "the second process to be ready", func() bool { return sd.readies() == 2 })
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len("between"))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "between" {
		t.Fatalf("what was sent between processes came back as %q: %v", got, err)
	}
	roundTrip("after")
	waitUntil(t, "the store to be let go", func() bool { return sd.stored() == 0 })

	// And a new player can still get in.
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("the listener did not survive: %v", err)
	}
	c2.Close()
}
