// Package version carries the one version string every binary here ships under.
//
// One number for the whole repo rather than one per binary. proxyd, proxybot,
// probed and triald are not independent programs: they share internal/tunnel,
// internal/control, internal/probe and internal/window, so a change to any of
// those moves several of them at once. Per-binary numbers would be four values
// derived from one commit, maintained by hand, that could only ever agree.
//
// V says what a release is called. It cannot say what is on a node — twenty-one
// commits once passed under an unchanged constant — so String() also reports the
// commit the binary was built from, which Go stamps into anything built inside
// the work tree. The two answer different questions: V is the name a release was
// announced under, the revision is the code that actually shipped.
package version

import "runtime/debug"

// V is bumped by editing this line. The README and the server list already call
// the current system v2 (v1 is minecraftspeedproxy, still holding 25565 on CH),
// and 2.0.0 was announced in Discord before any of this existed, so the
// numbering continues after that rather than starting at 0.1.0.
const V = "2.2.0"

// String renders the version with the revision it was built from: "v2.1.0
// (733d62d)", or "v2.1.0 (733d62d, dirty)" when the tree had uncommitted changes
// at build time. A binary built outside the work tree carries no revision and
// reports the bare "v2.1.0" — worth knowing in itself, since every binary this
// repo deploys is built inside it.
func String() string {
	rev, dirty := revision()
	switch {
	case rev == "":
		return "v" + V
	case dirty:
		return "v" + V + " (" + rev + ", dirty)"
	default:
		return "v" + V + " (" + rev + ")"
	}
}

// revision reads the stamp `go build` leaves on a binary built from a git work
// tree. Nothing here sets it: -buildvcs is on by default and survives the
// -trimpath that proxyctl builds with.
func revision() (rev string, dirty bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	return rev, dirty
}
