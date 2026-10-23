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
	"orchestrator/internal/volume"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Mounts translates declared volumes into Docker mounts. The Kubernetes
// counterpart is kube.PersistentVolumes.
func Mounts(vols []volume.Volume) []mount.Mount {
	mounts := make([]mount.Mount, 0, len(vols))
	for _, v := range vols {
		m := mount.Mount{Type: mount.TypeVolume, Source: v.Source, Target: v.Path, ReadOnly: v.ReadOnly}
		if v.SubPath != "" {
			m.VolumeOptions = &mount.VolumeOptions{Subpath: v.SubPath}
		}
		mounts = append(mounts, m)
	}
	return mounts
}

// PullImage makes sure an image is present locally, pulling it if it is not. A
// present image is left alone — no tag is re-resolved mid-flight, so a running
// workload and the next one started from the same tag are the same bytes.
func PullImage(ctx context.Context, cli *client.Client, ref string) error {
	if _, err := cli.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	pull, err := cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer pull.Close()
	return pull.Wait(ctx)
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
