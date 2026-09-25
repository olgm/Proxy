// Package handoff passes a running proxyd's sockets, and a snapshot of what it
// was doing with them, to the process systemd starts next.
//
// The vehicle is systemd's file descriptor store. A process that is about to go
// hands systemd a copy of every descriptor it wants kept, each under a name;
// systemd holds them across the restart and passes them to the next process as
// LISTEN_FDS. While systemd holds a copy nothing about the socket changes: a TCP
// connection is not closed, because its last descriptor never is, and datagrams
// keep arriving in a UDP socket's queue. So a player's connection outlives the
// process relaying it, and the next process carries on from the same bytes.
//
// It speaks the notify protocol directly rather than through libsystemd, because
// proxyd is stdlib-only: one datagram per message on $NOTIFY_SOCKET, with the
// descriptors attached as SCM_RIGHTS.
package handoff

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// snapshotName is the store entry holding the snapshot itself: a file, written
// and unlinked before it is stored, so it exists exactly as long as the store
// holds it and can never be read by a process it was not meant for.
const snapshotName = "snapshot"

// barrierTimeout bounds how long Store waits for systemd to confirm it has taken
// everything. Past it the process goes anyway: it has nothing left to do.
const barrierTimeout = 5 * time.Second

// Available reports whether this process can hand off at all: systemd started
// it with somewhere to send notifications and a store to put descriptors in.
func Available() bool {
	n, _ := strconv.Atoi(os.Getenv("FDSTORE"))
	return os.Getenv("NOTIFY_SOCKET") != "" && n > 0
}

// File is one descriptor to keep, under the name the next process will find it by.
// A name is printable ASCII, no colon, at most 255 bytes: systemd's rule.
type File struct {
	Name string
	File *os.File
}

// Store puts the snapshot and every file in systemd's store, and returns once
// systemd has taken them all. The files stay open here; the caller closes them,
// or simply exits, which is the point of handing them over.
func Store(snapshot []byte, files []File) error {
	c, err := dial()
	if err != nil {
		return err
	}
	defer c.Close()

	f, err := snapshotFile(snapshot)
	if err != nil {
		return err
	}
	defer f.Close()
	// The snapshot first: a store that has sockets but no snapshot would hand the
	// next process descriptors it could not make sense of, where one that has a
	// snapshot and some sockets missing loses only the sessions those carried.
	if err := keep(c, snapshotName, f); err != nil {
		return err
	}
	for _, e := range files {
		if err := keep(c, e.Name, e.File); err != nil {
			return fmt.Errorf("store %s: %w", e.Name, err)
		}
	}
	return barrier(c)
}

// Ready tells systemd the process has started, with a line for `systemctl status`.
func Ready(status string) error {
	return notify("READY=1\nSTATUS=" + status)
}

// Status updates the line `systemctl status` shows.
func Status(status string) error {
	return notify("STATUS=" + status)
}

// Forget removes entries from the store. A process that has taken over has its
// own copy of every descriptor, and as long as systemd holds another, closing a
// connection here would not close it at all: the far end would wait for a FIN
// that never comes.
func Forget(names []string) error {
	c, err := dial()
	if err != nil {
		return err
	}
	defer c.Close()
	for _, n := range names {
		if err := send(c, "FDSTOREREMOVE=1\nFDNAME="+n, nil); err != nil {
			return err
		}
	}
	return nil
}

// Inherited is what a previous process left: its snapshot, and the files it kept.
type Inherited struct {
	Snapshot []byte
	files    map[string]*os.File
	names    []string
}

// NewInherited is what Take would return for a snapshot and files that reached
// this process some other way. Tests use it to stand in for systemd.
func NewInherited(snapshot []byte, files []File) *Inherited {
	in := &Inherited{Snapshot: snapshot, files: map[string]*os.File{}, names: []string{snapshotName}}
	for _, f := range files {
		in.files[f.Name] = f.File
		in.names = append(in.names, f.Name)
	}
	return in
}

// Take claims the descriptors systemd passed this process, if it passed any, and
// clears the variables that described them. Nil means a clean start.
func Take() (*Inherited, error) {
	pid, count, names := os.Getenv("LISTEN_PID"), os.Getenv("LISTEN_FDS"), os.Getenv("LISTEN_FDNAMES")
	for _, k := range []string{"LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES"} {
		os.Unsetenv(k)
	}
	if count == "" || pid != strconv.Itoa(os.Getpid()) {
		return nil, nil
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("handoff: LISTEN_FDS=%q", count)
	}
	// systemd passes them from 3 up, in the order LISTEN_FDNAMES names them.
	fds := make([]int, n)
	for i := range fds {
		fds[i] = 3 + i
	}
	return inherit(strings.Split(names, ":"), fds)
}

func inherit(names []string, fds []int) (*Inherited, error) {
	in := &Inherited{files: map[string]*os.File{}}
	for i, fd := range fds {
		syscall.CloseOnExec(fd)
		name := ""
		if i < len(names) {
			name = names[i]
		}
		f := os.NewFile(uintptr(fd), name)
		in.names = append(in.names, name)
		if name != snapshotName {
			if old := in.files[name]; old != nil {
				old.Close()
			}
			in.files[name] = f
			continue
		}
		// Its offset is wherever the writer left it: descriptors share one.
		b, err := readFrom0(f)
		f.Close()
		if err != nil {
			in.Close()
			return nil, fmt.Errorf("handoff: snapshot: %w", err)
		}
		in.Snapshot = b
	}
	return in, nil
}

func readFrom0(f *os.File) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// File hands over the file stored under name, once: a second claim, or a name
// nobody stored, gets nil. The caller owns it from then on.
func (in *Inherited) File(name string) *os.File {
	f := in.files[name]
	delete(in.files, name)
	return f
}

// Names are every entry the store held, snapshot included: what Forget removes.
func (in *Inherited) Names() []string { return in.names }

// Close closes every file nobody claimed. A socket the new config has no use for
// ends here, as it would have in a plain restart.
func (in *Inherited) Close() {
	for name, f := range in.files {
		f.Close()
		delete(in.files, name)
	}
}

// notifier is an unbound datagram socket aimed at $NOTIFY_SOCKET. Raw rather
// than a net.UnixConn: the net package will not attach descriptors to a datagram
// on a connected socket, and there is nothing here for its poller to do.
type notifier struct {
	fd int
	to *syscall.SockaddrUnix
}

func dial() (*notifier, error) {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil, errors.New("handoff: not started by systemd with a notify socket")
	}
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("handoff: notify socket: %w", err)
	}
	syscall.CloseOnExec(fd)
	// A leading @ is an abstract name, which syscall understands as written.
	return &notifier{fd: fd, to: &syscall.SockaddrUnix{Name: path}}, nil
}

func (c *notifier) Close() error { return syscall.Close(c.fd) }

func notify(state string) error {
	if os.Getenv("NOTIFY_SOCKET") == "" {
		return nil // not under systemd: there is nobody to tell
	}
	c, err := dial()
	if err != nil {
		return err
	}
	defer c.Close()
	return send(c, state, nil)
}

// keep stores one descriptor. FDPOLL=0 because systemd otherwise drops an entry
// the moment its descriptor reports a hangup, and a half-closed connection with
// bytes still unread reports one.
func keep(c *notifier, name string, f *os.File) error {
	return send(c, "FDSTORE=1\nFDNAME="+name+"\nFDPOLL=0", f)
}

// send writes one notification, with f attached if there is one. The descriptor
// is reached through SyscallConn rather than Fd, because Fd puts a descriptor in
// blocking mode — and a socket's mode is shared by every copy of it, including the
// one the relay is still using.
func send(c *notifier, state string, f *os.File) error {
	if f == nil {
		return syscall.Sendmsg(c.fd, []byte(state), nil, c.to, 0)
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var werr error
	if err := rc.Control(func(fd uintptr) {
		werr = syscall.Sendmsg(c.fd, []byte(state), syscall.UnixRights(int(fd)), c.to, 0)
	}); err != nil {
		return err
	}
	return werr
}

// barrier waits until systemd has processed everything sent before it: it closes
// its copy of the pipe once it has, and the read end sees EOF. Without it the
// process could exit with its last descriptors still queued, and systemd reads a
// notification from a process that has gone as one it cannot place.
func barrier(c *notifier) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	err = send(c, "BARRIER=1", w)
	w.Close()
	if err != nil {
		return err
	}
	r.SetReadDeadline(time.Now().Add(barrierTimeout))
	if _, err := io.Copy(io.Discard, r); err != nil {
		return fmt.Errorf("handoff: systemd did not confirm the store: %w", err)
	}
	return nil
}

// snapshotFile writes the snapshot where the service may write, and unlinks it at
// once: from then on the only way to it is the descriptor.
func snapshotFile(b []byte) (*os.File, error) {
	dir, _, _ := strings.Cut(os.Getenv("STATE_DIRECTORY"), ":")
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, "handoff-*")
	if err != nil {
		return nil, err
	}
	os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
