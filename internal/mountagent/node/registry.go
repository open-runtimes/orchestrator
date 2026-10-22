// Package node is the node mount agent: a CSI node plugin that serves each
// mounting pod's workspace as an inline ephemeral volume, and the HTTP API
// through which that pod's sidecar asks for mounts inside it
// (docs/design/node-mount-agent.md). The contract it serves is
// internal/mountagent.
package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Volume is one published workspace.
type Volume struct {
	ID        string `json:"id"`
	Target    string `json:"target"`    // kubelet's publish path: the workspace as the pod sees it
	TokenHash string `json:"tokenHash"` // hex sha256 of the token; the token itself is never stored
	Pod       string `json:"pod"`       // namespace/name, for logs

	mu sync.Mutex // serializes operations within one workspace
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// registry indexes published volumes by ID (for CSI) and by token hash (for
// the mount API). Records are persisted beside each volume's data, so an agent
// restart keeps serving workspaces kubelet published before it.
type registry struct {
	dir string // <state>/volumes

	mu      sync.RWMutex
	byID    map[string]*Volume
	byToken map[string]*Volume
}

func newRegistry(stateDir string) (*registry, error) {
	r := &registry{
		dir:     filepath.Join(stateDir, "volumes"),
		byID:    map[string]*Volume{},
		byToken: map[string]*Volume{},
	}
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(r.dir, e.Name(), "volume.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue // a publish that never finished; kubelet retries or unpublishes it
		}
		v := &Volume{}
		if err == nil {
			err = json.Unmarshal(data, v)
		}
		if err != nil {
			// One unreadable record must not take down every workspace on the
			// node. Its pod's mount requests fail until kubelet republishes.
			slog.Warn("Skipping unreadable volume record", "dir", e.Name(), "error", err)
			continue
		}
		r.byID[v.ID] = v
		r.byToken[v.TokenHash] = v
	}
	return r, nil
}

// volumeDir is where a volume's record and data live, named by a hash of the
// kubelet-generated ID so it is always one safe path component.
func (r *registry) volumeDir(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(r.dir, hex.EncodeToString(sum[:16]))
}

func (r *registry) dataDir(id string) string { return filepath.Join(r.volumeDir(id), "data") }

// put records v. A token already bound to another volume is refused: a token
// read from one pod's spec must not let a second pod take its workspace's
// mapping, or the first pod's mounts would land in the second's.
func (r *registry) put(v *Volume) error {
	r.mu.RLock()
	owner := r.byToken[v.TokenHash]
	r.mu.RUnlock()
	if owner != nil && owner.ID != v.ID {
		return errors.New("token is already bound to another workspace")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(r.volumeDir(v.ID), "volume.json"), data); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[v.ID] = v
	r.byToken[v.TokenHash] = v
	return nil
}

func (r *registry) get(id string) *Volume {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[id]
}

// forToken resolves a presented token to its volume. The map is keyed by the
// token's hash, so the secret itself is never compared.
func (r *registry) forToken(token string) *Volume {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byToken[hashToken(token)]
}

func (r *registry) remove(id string) error {
	r.mu.Lock()
	if v := r.byID[id]; v != nil && r.byToken[v.TokenHash] == v {
		delete(r.byToken, v.TokenHash)
	}
	delete(r.byID, id)
	r.mu.Unlock()
	return os.RemoveAll(r.volumeDir(id))
}

// writeFileSync replaces path atomically and durably: a crash leaves either
// the old record or the new one, never an empty file.
func writeFileSync(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", filepath.Dir(path), err)
	}
	return nil
}
