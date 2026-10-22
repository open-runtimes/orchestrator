//go:build linux

package mountagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchestrator/internal/mount"
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

// Agent serves both halves: CSI (to kubelet) and the mount API (to sidecars).
type Agent struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer

	driver  string
	version string
	nodeID  string
	reg     *registry
}

// New loads the registry from stateDir and returns an agent for this node.
func New(driver, version, nodeID, stateDir string) (*Agent, error) {
	reg, err := newRegistry(stateDir)
	if err != nil {
		return nil, fmt.Errorf("load volumes: %w", err)
	}
	return &Agent{driver: driver, version: version, nodeID: nodeID, reg: reg}, nil
}

// RegisterCSI registers the Identity and Node services. There is no Controller
// service: inline ephemeral volumes are created and destroyed on the node.
func (a *Agent) RegisterCSI(s *grpc.Server) {
	csi.RegisterIdentityServer(s, a)
	csi.RegisterNodeServer(s, a)
}

// --- CSI Identity ---

func (a *Agent) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: a.driver, VendorVersion: a.version}, nil
}

func (a *Agent) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{}, nil
}

func (a *Agent) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

// --- CSI Node ---

func (a *Agent) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (a *Agent) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: a.nodeID}, nil
}

// NodePublishVolume creates an empty workspace and binds it at kubelet's
// target as a shared mount. Shared is what lets a mount the agent later stacks
// inside the target propagate into containers that mount the volume with
// HostToContainer — without relying on the node's root mount being shared.
func (a *Agent) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	id, target, attrs := req.GetVolumeId(), req.GetTargetPath(), req.GetVolumeContext()
	if id == "" || target == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and target path are required")
	}
	if attrs["csi.storage.k8s.io/ephemeral"] != "true" {
		return nil, status.Error(codes.InvalidArgument, "only inline ephemeral volumes are supported")
	}
	token := attrs[AttributeToken]
	if len(token) < minTokenLen {
		return nil, status.Errorf(codes.InvalidArgument, "volume attribute %q must carry a token of at least %d characters", AttributeToken, minTokenLen)
	}

	if v := a.reg.get(id); v != nil && isMountPoint(target) {
		return &csi.NodePublishVolumeResponse{}, nil // kubelet retried a publish that succeeded
	}

	data := filepath.Join(a.reg.volumeDir(id), "data")
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
	if !isMountPoint(target) {
		if err := unix.Mount(data, target, "", unix.MS_BIND, ""); err != nil {
			return nil, status.Errorf(codes.Internal, "bind workspace: %v", err)
		}
	}
	if err := unix.Mount("", target, "", unix.MS_SHARED, ""); err != nil {
		_ = unix.Unmount(target, unix.MNT_DETACH)
		return nil, status.Errorf(codes.Internal, "share workspace: %v", err)
	}

	v := &Volume{
		ID: id, Target: filepath.Clean(target), Data: data, TokenHash: hashToken(token),
		Pod: attrs["csi.storage.k8s.io/pod.namespace"] + "/" + attrs["csi.storage.k8s.io/pod.name"], PodUID: attrs["csi.storage.k8s.io/pod.uid"],
	}
	if err := a.reg.put(v); err != nil {
		_ = unix.Unmount(target, unix.MNT_DETACH)
		return nil, status.Errorf(codes.Internal, "record volume: %v", err)
	}
	slog.Info("Published workspace", "volume", id, "pod", v.Pod)
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume takes every artifact mount out of the workspace,
// innermost first, then the workspace bind itself, and deletes the data. It
// runs after the pod's containers have stopped, so nothing the workload held
// keeps a mount busy; a mount that still is fails the call, and kubelet
// retries rather than finding a busy directory in its own teardown.
func (a *Agent) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	id, target := req.GetVolumeId(), filepath.Clean(req.GetTargetPath())
	if id == "" || target == "." {
		return nil, status.Error(codes.InvalidArgument, "volume id and target path are required")
	}
	v := a.reg.get(id)
	if v != nil {
		v.mu.Lock()
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

// teardown unmounts everything at or below target. Mounts inside the
// workspace are reached through a Root, because the workload named their paths
// and could have replaced a parent with a symlink since; mountinfo is re-read
// each round in case a mount was moved. The workspace bind at target is
// kubelet's own path and is unmounted last, by name.
func teardown(target string) error {
	deadline := time.Now().Add(10 * time.Second)
	for range 5 {
		data, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			return err
		}
		var inner []string
		for _, m := range mount.MountsUnder(string(data), target) {
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
		for i := len(inner) - 1; i >= 0; i-- {
			// A failure here (moved, swapped, still busy) is retried: the next
			// round re-reads mountinfo.
			if err := root.Unmount(inner[i], deadline); err != nil {
				slog.Warn("Unmount during teardown failed", "path", inner[i], "error", err)
			}
		}
		root.Close()
	}
	return fmt.Errorf("mounts remain under %s", target)
}

func isMountPoint(path string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	path = filepath.Clean(path)
	return slices.Contains(mount.MountsUnder(string(data), path), path)
}

// --- Mount API ---

// Handler serves the mount API. Every request carries a volume's token, which
// selects the one workspace it may act in; paths are relative to it.
func (a *Agent) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mount", a.serve(func(b mount.Beneath, body []byte) (any, error) {
		var req MountRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, badRequest(err)
		}
		return struct{}{}, b.Mount(req.Source, req.Target, mount.Opts{
			Writable: req.Writable, SizeMiB: req.SizeMiB, SourceDir: req.SourceDir, UpperOnDisk: req.UpperOnDisk,
		})
	}))
	mux.HandleFunc("POST /v1/unmount", a.serve(func(b mount.Beneath, body []byte) (any, error) {
		var req TargetRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, badRequest(err)
		}
		return struct{}{}, b.Unmount(req.Target)
	}))
	mux.HandleFunc("POST /v1/mounted", a.serve(func(b mount.Beneath, body []byte) (any, error) {
		var req TargetRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, badRequest(err)
		}
		mounted, err := b.IsMounted(req.Target)
		return MountedResponse{Mounted: mounted}, err
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

type badRequestError struct{ error }

func badRequest(err error) error { return badRequestError{err} }

func (a *Agent) serve(op func(mount.Beneath, []byte) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		v := a.reg.forToken(token)
		if v == nil {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unknown workspace token"})
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}

		v.mu.Lock()
		defer v.mu.Unlock()
		start := time.Now()
		root, err := mount.OpenRoot(v.Target)
		if err != nil {
			writeJSON(w, http.StatusConflict, errorResponse{Error: "workspace is not published: " + err.Error()})
			return
		}
		defer root.Close()
		resp, err := op(mount.Beneath{Root: root}, body)
		log := slog.With("op", r.URL.Path, "pod", v.Pod, "volume", v.ID, "duration", time.Since(start))
		switch {
		case errors.As(err, &badRequestError{}):
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		case err != nil:
			log.Warn("Mount request failed", "error", err)
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: err.Error()})
		default:
			log.Info("Mount request served")
			writeJSON(w, http.StatusOK, resp)
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
