package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNode is a directory standing in for a node: the binaries in bin, and a
// systemctl and a kill on PATH that act on a few state files instead of a real
// unit. onUSR2 is what the running proxyd does when it is told to hand off.
type fakeNode struct {
	dir, bin string
}

func newFakeNode(t *testing.T, status, onUSR2, onStart string) *fakeNode {
	t.Helper()
	dir := t.TempDir()
	f := &fakeNode{dir: dir, bin: filepath.Join(dir, "bin")}
	os.MkdirAll(f.bin, 0o755)
	write := func(name, body string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("bin/proxyd", "old", 0o644)
	write("bin/proxyd.next", "new", 0o644)
	os.MkdirAll(filepath.Join(dir, "etc"), 0o755)
	write("etc/config.json", "new config", 0o644)
	write("etc/config.json.prev", "old config", 0o644)
	write("pid", "100", 0o644)
	write("active", "active", 0o644)
	write("status", status, 0o644)
	write("log", "", 0o644)
	os.MkdirAll(filepath.Join(dir, "path"), 0o755)
	write("path/systemctl", `#!/bin/sh
D=`+dir+`
echo "systemctl $*" >> $D/log
case "$*" in
  "show -p MainPID --value proxyd") cat $D/pid ;;
  "show -p StatusText --value proxyd") cat $D/status ;;
  "is-active proxyd") cat $D/active; [ "$(cat $D/active)" = active ] ;;
  "start --no-block proxyd") `+onStart+` ;;
  *) : ;;
esac
`, 0o755)
	write("path/kill", `#!/bin/sh
D=`+dir+`
echo "kill $*" >> $D/log
case "$1" in
  -USR2) `+onUSR2+` ;;
esac
`, 0o755)
	write("path/sleep", "#!/bin/sh\n", 0o755)
	return f
}

// run runs swapScript on the fake node, with env standing in for sudo so kill
// is looked up on PATH rather than being the shell's own.
func (f *fakeNode) run(t *testing.T) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", "set -eu\nSUDO=env\n"+swapScript(f.bin, filepath.Join(f.dir, "etc")))
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(f.dir, "path")+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *fakeNode) read(t *testing.T, name string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, name))
	return string(b)
}

// A proxyd that can hand off gets SIGUSR2, not a restart, and the new binary is
// in place once the new process is up.
func TestSwapHandsOffToANewProcess(t *testing.T) {
	f := newFakeNode(t, "handoff ready", `echo 0 > $D/pid; echo activating > $D/active; echo 101 > $D/pid; echo active > $D/active`, ":")
	out, err := f.run(t)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "handoff: sessions carried from pid 100 to 101") {
		t.Fatalf("output:\n%s", out)
	}
	if f.read(t, "bin/proxyd") != "new" || f.read(t, "bin/proxyd.prev") != "old" {
		t.Fatal("the new binary is not in place with the old one kept beside it")
	}
	if log := f.read(t, "log"); strings.Contains(log, "restart") || !strings.Contains(log, "kill -USR2 100") {
		t.Fatalf("expected a SIGUSR2 and no restart:\n%s", log)
	}
}

// A new proxyd that never comes up is killed, the old binary and its config go
// back, and the old binary takes the store back. The deploy fails, so it goes no
// further.
func TestSwapRollsBackWhenTheNewProcessDoesNotComeUp(t *testing.T) {
	// The first start is the one the script kicks off at once; the new binary
	// never gets to ready. The second, after the rollback, is the old binary.
	f := newFakeNode(t, "handoff ready",
		`echo 101 > $D/pid; echo activating > $D/active`,
		`if [ -f $D/kicked ]; then echo 102 > $D/pid; echo active > $D/active; else touch $D/kicked; fi`)
	out, err := f.run(t)
	if err == nil {
		t.Fatalf("a failed handoff did not fail the deploy:\n%s", out)
	}
	if !strings.Contains(out, "rolled back; the previous binary carried the sessions") {
		t.Fatalf("output:\n%s", out)
	}
	if f.read(t, "bin/proxyd") != "old" || f.read(t, "etc/config.json") != "old config" {
		t.Fatal("the old binary and config were not put back")
	}
	log := f.read(t, "log")
	if !strings.Contains(log, "kill -KILL 101") || !strings.Contains(log, "systemctl start --no-block proxyd") {
		t.Fatalf("the stuck process was not replaced:\n%s", log)
	}
}

// A proxyd that takes SIGUSR2 and does not go — it had no store to hand off to —
// is left running, and the files it was started from go back.
func TestSwapLeavesAnOldProxydThatDidNotHandOff(t *testing.T) {
	f := newFakeNode(t, "handoff ready", ":", ":")
	out, err := f.run(t)
	if err == nil || !strings.Contains(out, "never handed off, and is still running") {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.read(t, "bin/proxyd") != "old" || f.read(t, "etc/config.json") != "old config" {
		t.Fatal("the old binary and config were not put back")
	}
	if log := f.read(t, "log"); strings.Contains(log, "KILL") || strings.Contains(log, "start") {
		t.Fatalf("a running proxyd was disturbed:\n%s", log)
	}
}

// The new process is started at once when the old one goes, not after the
// unit's two-second crash-loop delay.
func TestSwapStartsTheNewProcessAtOnce(t *testing.T) {
	f := newFakeNode(t, "handoff ready", `echo 0 > $D/pid; echo activating > $D/active`,
		`echo 101 > $D/pid; echo active > $D/active`)
	out, err := f.run(t)
	if err != nil || !strings.Contains(out, "sessions carried from pid 100 to 101") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// A proxyd that cannot hand off — anything before this, or one started without
// a store — is restarted, as every deploy used to do.
func TestSwapRestartsWhatCannotHandOff(t *testing.T) {
	f := newFakeNode(t, "", ":", ":")
	out, err := f.run(t)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.read(t, "bin/proxyd") != "new" {
		t.Fatal("the new binary is not in place")
	}
	if log := f.read(t, "log"); !strings.Contains(log, "systemctl restart proxyd") || strings.Contains(log, "kill") {
		t.Fatalf("expected a restart and no signal:\n%s", log)
	}
}

// The unit is what makes a handoff possible at all: systemd has to wait for
// READY, keep descriptors, keep them through a failure, restart at once, and
// let proxyd reach its notify socket.
func TestUnitAllowsAHandoff(t *testing.T) {
	script := installScript("proxyd", true, false, false, "")
	for _, want := range []string{
		"Type=notify", "NotifyAccess=main", "FileDescriptorStoreMax=8192",
		"FileDescriptorStorePreserve=yes", "RestartMode=direct", "RestartSec=2",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("unit lacks %s", want)
		}
	}
	if out, err := exec.Command("bash", "-n", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("install script does not parse: %v\n%s", err, out)
	}
}
