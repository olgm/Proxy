//go:build linux

package proxy

import (
	"encoding/binary"
	"net"
	"syscall"
	"time"
	"unsafe"
)

// Offsets into struct tcp_info. The kernel only ever appends to it, so these hold
// on every kernel new enough to have the last of them.
const (
	tcpiRTT          = 68  // tcpi_rtt, smoothed, µs
	tcpiRTTVar       = 72  // tcpi_rttvar, µs
	tcpiTotalRetrans = 100 // tcpi_total_retrans, segments
	tcpiMinRTT       = 148 // tcpi_min_rtt, µs; Linux 4.6
	tcpiLen          = tcpiMinRTT + 4
)

// tcpInfo reads the kernel's own account of a connection: its smoothed round trip
// and deviation, the lowest round trip it has seen, and how many segments it has
// sent again. The kernel keeps these for congestion control anyway, so reading
// them is one system call and sends nothing — no probe of any kind reaches the
// player.
//
// syscall has no TCP_INFO reader and proxyd stays stdlib-only, so this decodes the
// struct by offset.
func tcpInfo(c *net.TCPConn) (tcpSample, bool) {
	rc, err := c.SyscallConn()
	if err != nil {
		return tcpSample{}, false
	}
	var b [256]byte
	n := uint32(len(b))
	var errno syscall.Errno
	err = rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd,
			syscall.IPPROTO_TCP, syscall.TCP_INFO,
			uintptr(unsafe.Pointer(&b[0])), uintptr(unsafe.Pointer(&n)), 0)
	})
	// A kernel that returns less is older than any node here, and says nothing
	// rather than something wrong.
	if err != nil || errno != 0 || n < tcpiLen {
		return tcpSample{}, false
	}
	u32 := func(off int) uint32 { return binary.NativeEndian.Uint32(b[off:]) }
	us := func(off int) time.Duration { return time.Duration(u32(off)) * time.Microsecond }
	s := tcpSample{rtt: us(tcpiRTT), rttvar: us(tcpiRTTVar), retrans: u32(tcpiTotalRetrans)}
	// ~0 until the first round trip has been timed.
	if m := u32(tcpiMinRTT); m != ^uint32(0) {
		s.minRTT = time.Duration(m) * time.Microsecond
	}
	return s, true
}
