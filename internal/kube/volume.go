package kube

import (
	"fmt"
	"orchestrator/pkg/volume"

	corev1 "k8s.io/api/core/v1"
)

// PersistentVolumes converts workload volumes into the pod-level Volumes (each a
// PersistentVolumeClaim reference) and the matching worker VolumeMounts. Volume
// names are index-derived so two mounts of the same claim (e.g. different
// subPaths) never collide. Returns nil, nil when there are no volumes.
func PersistentVolumes(vols []volume.Volume) ([]corev1.Volume, []corev1.VolumeMount) {
	if len(vols) == 0 {
		return nil, nil
	}
	podVolumes := make([]corev1.Volume, 0, len(vols))
	mounts := make([]corev1.VolumeMount, 0, len(vols))
	for i, v := range vols {
		name := fmt.Sprintf("vol-%d", i)
		podVolumes = append(podVolumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: v.Source,
					ReadOnly:  v.ReadOnly,
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name:      name,
			MountPath: v.Path,
			SubPath:   v.SubPath,
			ReadOnly:  v.ReadOnly,
		})
	}
	return podVolumes, mounts
}
