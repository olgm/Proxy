//go:build !linux

package proxy

import "net"

// tcpInfo has nothing to read off Linux. Every node is Linux; a build for anything
// else records no round trip.
func tcpInfo(*net.TCPConn) (tcpSample, bool) { return tcpSample{}, false }
