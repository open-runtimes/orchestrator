//go:build linux

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchestrator/internal/mount"
	"orchestrator/internal/mountagent"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// minTokenLen rejects a token too short to be the one the mapper mints
// (crypto/rand.Text: 26 base32 characters, 130 bits).
const minTokenLen = 26

// Agent serves both halves: CSI (to kubelet) and the mount API (to sidecars).
type Agent struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer

	version    string
	nodeID     string
	namespaces []string // pods outside these may not use the driver; empty = any
	reg        *registry
}

// New loads the registry from stateDir and returns an agent for this node.
func New(version, nodeID, stateDir string, namespaces []string) (*Agent, error) {
	reg, err := newRegistry(stateDir)
	if err != nil {
		return nil, fmt.Errorf("load volumes: %w", err)
	}
	return &Agent{version: version, nodeID: nodeID, namespaces: namespaces, reg: reg}, nil
}

// RegisterCSI registers the Identity and Node services. There is no Controller
// service: inline ephemeral volumes are created and destroyed on the node.
func (a *Agent) RegisterCSI(s *grpc.Server) {
	csi.RegisterIdentityServer(s, a)
	csi.RegisterNodeServer(s, a)
}

func (a *Agent) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: mountagent.Driver, VendorVersion: a.version}, nil
}

func (a *Agent) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (a *Agent) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

func (a *Agent) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (a *Agent) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: a.nodeID}, nil
}

// NodePublishVolume creates an empty workspace and binds it at kubelet's
// target: nodev and nosuid, because nothing in a workspace is a device or a
// setuid binary the agent should ever honour, and shared, so mounts the agent
// later stacks inside it propagate into containers that mount the volume with
// HostToContainer.
func (a *Agent) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	id, target, attrs := req.GetVolumeId(), req.GetTargetPath(), req.GetVolumeContext()
	if id == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and target path are required")
	}
	if attrs["csi.storage.k8s.io/ephemeral"] != "true" {
		return nil, status.Error(codes.InvalidArgument, "only inline ephemeral volumes are supported")
	}
	ns := attrs["csi.storage.k8s.io/pod.namespace"]
	if len(a.namespaces) > 0 && !slices.Contains(a.namespaces, ns) {
		return nil, status.Errorf(codes.PermissionDenied, "namespace %q may not use %s", ns, mountagent.Driver)
	}
	token := attrs[mountagent.AttributeToken]
	if len(token) < minTokenLen {
		return nil, status.Errorf(codes.InvalidArgument, "volume attribute %q must carry a token", mountagent.AttributeToken)
	}
	if v := a.reg.get(id); v != nil {
		if mounted, _ := mount.IsMountPoint(target); mounted {
			return &csi.NodePublishVolumeResponse{}, nil // kubelet retried a publish that succeeded
		}
	}

	data := a.reg.dataDir(id)
	if err := os.MkdirAll(data, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "create workspace: %v", err)
	}
	// World-writable, like the emptyDir it replaces: the workload's uid is not
	// known here.
	if err := os.Chmod(data, 0o777); err != nil {
		return nil, status.Errorf(codes.Internal, "chmod workspace: %v", err)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		return nil, status.Errorf(codes.Internal, "create target: %v", err)
	}
	if mounted, _ := mount.IsMountPoint(target); !mounted {
		if err := unix.Mount(data, target, "", unix.MS_BIND, ""); err != nil {
			return nil, status.Errorf(codes.Internal, "bind workspace: %v", err)
		}
	}
	for _, flags := range []uintptr{unix.MS_BIND | unix.MS_REMOUNT | unix.MS_NODEV | unix.MS_NOSUID, unix.MS_SHARED} {
		if err := unix.Mount("", target, "", flags, ""); err != nil {
			_ = unix.Unmount(target, unix.MNT_DETACH)
			return nil, status.Errorf(codes.Internal, "configure workspace: %v", err)
		}
	}

	v := &Volume{ID: id, Target: filepath.Clean(target), TokenHash: hashToken(token), Pod: ns + "/" + attrs["csi.storage.k8s.io/pod.name"]}
	if err := a.reg.put(v); err != nil {
		_ = unix.Unmount(target, unix.MNT_DETACH)
		return nil, status.Errorf(codes.FailedPrecondition, "record volume: %v", err)
	}
	slog.Info("Published workspace", "volume", id, "pod", v.Pod)
	return &csi.NodePublishVolumeResponse{}, nil
}

// unpublishLockWait bounds how long unpublish waits for a mount request in
// flight on the same workspace; kubelet retries a call that gives up.
const unpublishLockWait = 30 * time.Second

// NodeUnpublishVolume takes every artifact mount out of the workspace,
// innermost first, then the workspace bind itself, and deletes the data. It
// runs after the pod's containers have stopped, so nothing the workload held
// keeps a mount busy; if one still is, the call fails and kubelet retries
// rather than finding a busy directory in its own teardown.
func (a *Agent) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	id, target := req.GetVolumeId(), filepath.Clean(req.GetTargetPath())
	if id == "" || target == "." {
		return nil, status.Error(codes.InvalidArgument, "volume id and target path are required")
	}
	v := a.reg.get(id)
	if v != nil {
		if !lockWithin(&v.mu, unpublishLockWait) {
			return nil, status.Error(codes.Unavailable, "a mount request is still running in this workspace")
		}
		defer v.mu.Unlock()
	}
	if err := teardown(target); err != nil {
		return nil, status.Errorf(codes.Internal, "tear down workspace: %v", err)
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, status.Errorf(codes.Internal, "remove target: %v", err)
	}
	if err := a.reg.remove(id); err != nil {
		return nil, status.Errorf(codes.Internal, "remove workspace: %v", err)
	}
	if v != nil {
		slog.Info("Unpublished workspace", "volume", id, "pod", v.Pod)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func lockWithin(mu interface{ TryLock() bool }, wait time.Duration) bool {
	for deadline := time.Now().Add(wait); ; time.Sleep(50 * time.Millisecond) {
		if mu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// teardown unmounts everything at or below target. Mounts inside the
// workspace are reached through a Root, because the workload named their paths
// and could have replaced a parent with a symlink since; mountinfo is re-read
// each round in case a mount was moved. The workspace bind at target is
// kubelet's own path and is unmounted last, by name.
func teardown(target string) error {
	deadline := time.Now().Add(10 * time.Second)
	for range 5 {
		info, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			return err
		}
		var inner []string
		for _, m := range mount.MountsUnder(string(info), target) {
			if m != target {
				inner = append(inner, strings.TrimPrefix(m, target+"/"))
			}
		}
		if len(inner) == 0 {
			return mount.UnmountPath(target, deadline)
		}
		root, err := mount.OpenRoot(target)
		if err != nil {
			return err
		}
		for _, i := range slices.Backward(inner) {
			// A failure here (moved, swapped, still busy) is retried: the next
			// round re-reads mountinfo.
			if err := root.Unmount(i, deadline); err != nil {
				slog.Warn("Unmount during teardown failed", "path", i, "error", err)
			}
		}
		root.Close()
	}
	return fmt.Errorf("mounts remain under %s", target)
}

// Handler serves the mount API. Every request carries a volume's token, which
// selects the one workspace it may act in; paths are relative to it.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mount", serve(a, func(b mount.Beneath, req mountagent.MountRequest) (any, error) {
		return struct{}{}, b.Mount(req.Source, req.Target, req.Opts)
	}))
	mux.HandleFunc("POST /v1/unmount", serve(a, func(b mount.Beneath, req mountagent.TargetRequest) (any, error) {
		return struct{}{}, b.Unmount(req.Target)
	}))
	mux.HandleFunc("POST /v1/mounted", serve(a, func(b mount.Beneath, req mountagent.TargetRequest) (any, error) {
		mounted, err := b.IsMounted(req.Target)
		return mountagent.MountedResponse{Mounted: mounted}, err
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func serve[T any](a *Agent, op func(mount.Beneath, T) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		v := a.reg.forToken(token)
		if v == nil {
			writeJSON(w, http.StatusUnauthorized, mountagent.ErrorResponse{Error: "unknown workspace token"})
			return
		}
		var req T
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, mountagent.ErrorResponse{Error: err.Error()})
			return
		}

		v.mu.Lock()
		defer v.mu.Unlock()
		start := time.Now()
		var resp any
		err := a.published(v)
		if err == nil {
			resp, err = op(mount.Beneath{Dir: v.Target}, req)
		}
		log := slog.With("op", r.URL.Path, "pod", v.Pod, "volume", v.ID, "duration", time.Since(start))
		if err != nil {
			log.Warn("Mount request failed", "error", err)
			writeJSON(w, http.StatusBadRequest, mountagent.ErrorResponse{Error: err.Error()})
			return
		}
		log.Info("Mount request served")
		writeJSON(w, http.StatusOK, resp)
	}
}

// published checks that v's target is still the agent's bind of v's data. A
// record can outlive its bind (a node reboot before kubelet republishes), and
// mounting then would land in kubelet's directory on the node's own disk.
func (a *Agent) published(v *Volume) error {
	var target, data unix.Stat_t
	if err := unix.Stat(v.Target, &target); err != nil {
		return fmt.Errorf("workspace is not published: %w", err)
	}
	if err := unix.Stat(a.reg.dataDir(v.ID), &data); err != nil {
		return fmt.Errorf("workspace is not published: %w", err)
	}
	if target.Dev != data.Dev || target.Ino != data.Ino {
		return errors.New("workspace is not published")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
