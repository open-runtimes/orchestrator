// Package moby is the shared Docker-side plumbing: the counterpart to
// internal/kube, for the parts of talking to the engine that are the same
// whatever workload is being run — translating our volumes into mounts, making
// sure an image is present, and attaching to the configured network.
//
// Named for the engine's own project because the obvious names are taken: the
// three per-domain adapters are each `package docker`, and `container`,
// `network`, `image` and `mount` are all packages of the Docker SDK these files
// import.
package moby

import (
	"context"
	"fmt"
	"io"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/volume"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
)

// SubpathMinAPIVersion is the first Engine API version that honours a volume
// mount's Subpath (Docker Engine 26). Older daemons drop the field without an
// error and mount the whole volume.
const SubpathMinAPIVersion = "1.45"

// APIVersioner is the part of the Docker client Mounts needs: the API version
// negotiated with the daemon.
type APIVersioner interface {
	NegotiateAPIVersion(ctx context.Context)
	ClientVersion() string
}

// Mounts translates declared volumes into Docker mounts. The Kubernetes
// counterpart is kube.PersistentVolumes.
//
// A volume with a subPath is refused when the daemon speaks an API older than
// SubpathMinAPIVersion: it would ignore the subPath and give the container the
// whole volume, which is wider than anything the request asked for.
func Mounts(ctx context.Context, cli APIVersioner, vols []volume.Volume) ([]mount.Mount, error) {
	mounts := make([]mount.Mount, 0, len(vols))
	for i, v := range vols {
		m := mount.Mount{Type: mount.TypeVolume, Source: v.Source, Target: v.Path, ReadOnly: v.ReadOnly}
		if v.SubPath != "" {
			cli.NegotiateAPIVersion(ctx)
			if apiVersion := cli.ClientVersion(); versions.LessThan(apiVersion, SubpathMinAPIVersion) {
				return nil, apperrors.Validation(fmt.Sprintf("volumes[%d].subPath", i),
					fmt.Sprintf("subPath needs Docker Engine API %s or newer (Docker 26+); this daemon speaks %s and would mount the whole volume", SubpathMinAPIVersion, apiVersion))
			}
			m.VolumeOptions = &mount.VolumeOptions{Subpath: v.SubPath}
		}
		mounts = append(mounts, m)
	}
	return mounts, nil
}

// PullImage makes sure an image is present locally, pulling it if it is not. A
// present image is left alone — no tag is re-resolved mid-flight, so a running
// workload and the next one started from the same tag are the same bytes.
func PullImage(ctx context.Context, cli *client.Client, ref string) error {
	if _, err := cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	reader, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return err
	}
	defer reader.Close()
	// The pull only completes once its progress stream is drained.
	_, err = io.Copy(io.Discard, reader)
	return err
}

// NetworkingConfig attaches a container to the configured network, or returns
// nil for the default bridge when no network is configured.
func NetworkingConfig(name string) *network.NetworkingConfig {
	if name == "" {
		return nil
	}
	return &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{name: {}},
	}
}

// WorkloadSecurity is the Docker counterpart of kube.WorkloadSecurityContext,
// for a container running the user's own command (a build, a deployment, a
// sandbox): it keeps the image's user and a writable filesystem, since
// installers run as root and write everywhere, but drops CAP_NET_RAW — on a
// shared bridge a raw socket reaches neighbouring containers' traffic — and
// forbids gaining privileges through setuid binaries. The daemon applies its
// default seccomp profile on its own.
func WorkloadSecurity(hc *container.HostConfig) {
	hc.CapDrop = append(hc.CapDrop, "NET_RAW")
	hc.SecurityOpt = append(hc.SecurityOpt, "no-new-privileges")
}

// FileAccessSecurity is the Docker counterpart of
// kube.FileAccessSecurityContext, for the artifact sidecar: root with only the
// capabilities needed to read, write and re-own files of any owner or mode in
// the shared workspace, and no privilege gain.
func FileAccessSecurity(hc *container.HostConfig) {
	hc.CapDrop = append(hc.CapDrop, "ALL")
	hc.CapAdd = append(hc.CapAdd, "CHOWN", "DAC_OVERRIDE", "FOWNER")
	hc.SecurityOpt = append(hc.SecurityOpt, "no-new-privileges")
}
