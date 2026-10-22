package node

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// Records survive an agent restart, so a workspace kubelet published before
// the restart can still be mounted into and torn down after it.
func TestRegistryPersists(t *testing.T) {
	state := t.TempDir()
	reg, err := newRegistry(state)
	if err != nil {
		t.Fatal(err)
	}
	token := rand.Text()
	v := &Volume{ID: "csi-abc", Target: "/t", TokenHash: hashToken(token), Pod: "ns/p"}
	if err := os.MkdirAll(reg.volumeDir(v.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := reg.put(v); err != nil {
		t.Fatal(err)
	}

	reloaded, err := newRegistry(state)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.forToken(token); got == nil || got.Target != "/t" {
		t.Fatalf("reloaded registry lost the volume: %+v", got)
	}
	if err := reloaded.remove(v.ID); err != nil {
		t.Fatal(err)
	}
	if reloaded.forToken(token) != nil || reloaded.get(v.ID) != nil {
		t.Fatal("removed volume still resolves")
	}
	if _, err := os.Stat(reloaded.volumeDir(v.ID)); !os.IsNotExist(err) {
		t.Fatalf("volume directory left behind: %v", err)
	}
}

// A token stays with the volume that published it: another volume cannot take
// it over, and removing that other volume cannot drop it.
func TestRegistryKeepsTokenWithItsVolume(t *testing.T) {
	reg, err := newRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := hashToken(rand.Text())
	first, second := &Volume{ID: "first", TokenHash: h}, &Volume{ID: "second", TokenHash: h}
	for _, v := range []*Volume{first, second} {
		if err := os.MkdirAll(reg.volumeDir(v.ID), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := reg.put(first); err != nil {
		t.Fatal(err)
	}
	if err := reg.put(second); err == nil {
		t.Fatal("a second volume took over a token")
	}
	if err := reg.remove("second"); err != nil {
		t.Fatal(err)
	}
	if got := reg.byToken[h]; got != first {
		t.Fatalf("token now resolves to %+v, want the first volume", got)
	}
}

// One unreadable record cannot stop the agent from serving every other
// workspace on the node.
func TestRegistrySkipsCorruptRecords(t *testing.T) {
	state := t.TempDir()
	reg, err := newRegistry(state)
	if err != nil {
		t.Fatal(err)
	}
	good := &Volume{ID: "good", TokenHash: hashToken(rand.Text())}
	if err := os.MkdirAll(reg.volumeDir(good.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := reg.put(good); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(state, "volumes", "torn")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "volume.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newRegistry(state)
	if err != nil {
		t.Fatalf("a corrupt record stopped the registry: %v", err)
	}
	if reloaded.get("good") == nil {
		t.Fatal("the good record was lost")
	}
}
