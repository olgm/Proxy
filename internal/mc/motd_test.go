package mc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func render(t *testing.T, m *Motd, proto int32, online int) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(m.Render(proto, online)), &got); err != nil {
		t.Fatalf("render is not JSON: %v", err)
	}
	return got
}

// The client shows a red incompatible badge whenever the protocol it is told does
// not match its own, so the one field that cannot come from the file is the one
// naming a version.
func TestMotdEchoesClientProtocol(t *testing.T) {
	m, err := LoadMotd("")
	if err != nil {
		t.Fatalf("LoadMotd: %v", err)
	}
	for _, proto := range []int32{47, 340, 765, 767} {
		got := render(t, m, proto, 3)
		version := got["version"].(map[string]any)
		if version["protocol"].(float64) != float64(proto) {
			t.Errorf("proto %d rendered as %v", proto, version["protocol"])
		}
		if got["players"].(map[string]any)["online"].(float64) != 3 {
			t.Errorf("online count not rendered for proto %d", proto)
		}
	}
}

// The built-in document is neutral: no favicon, no players sample, generic
// description, so a fresh operator's install carries no prior branding.
func TestDefaultMotdIsNeutral(t *testing.T) {
	m, _ := LoadMotd("")
	got := render(t, m, 47, 1)
	if d := got["description"].(map[string]any)["text"]; d != "Minecraft proxy" {
		t.Errorf("description %q", d)
	}
	if _, ok := got["favicon"]; ok {
		t.Errorf("favicon present in default motd")
	}
	if _, ok := got["players"].(map[string]any)["sample"]; ok {
		t.Errorf("players sample present in default motd")
	}
}

// Render must not write through to the loaded document: two clients on different
// versions are rendered concurrently, and the second must not see the first's.
func TestRenderDoesNotMutateBase(t *testing.T) {
	m, _ := LoadMotd("")
	render(t, m, 765, 99)
	got := render(t, m, 47, 1)
	version := got["version"].(map[string]any)
	if version["protocol"].(float64) != 47 {
		t.Errorf("protocol leaked between renders: %v", version["protocol"])
	}
	if got["players"].(map[string]any)["online"].(float64) != 1 {
		t.Errorf("online count leaked between renders")
	}
}

func TestLoadMotdFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "motd.json")
	os.WriteFile(path, []byte(`{"description":{"text":"custom"},"players":{"max":42}}`), 0o644)
	m, err := LoadMotd(path)
	if err != nil {
		t.Fatalf("LoadMotd: %v", err)
	}
	got := render(t, m, 47, 7)
	if got["description"].(map[string]any)["text"] != "custom" {
		t.Errorf("file description not used: %v", got["description"])
	}
	players := got["players"].(map[string]any)
	if players["max"].(float64) != 42 || players["online"].(float64) != 7 {
		t.Errorf("players %v", players)
	}
}

// A missing or unparseable file must fail loudly at startup rather than leave the
// ingress with nothing to answer a status ping with.
func TestLoadMotdRejectsBadFile(t *testing.T) {
	if _, err := LoadMotd(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("missing file accepted")
	}
	path := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(path, []byte("{not json"), 0o644)
	if _, err := LoadMotd(path); err == nil {
		t.Error("unparseable file accepted")
	}
}
