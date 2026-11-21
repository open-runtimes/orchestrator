package kube

import (
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["test"] = &clientcmdapi.Cluster{Server: "https://kubernetes.test:6443"}
	kubeconfig.AuthInfos["test"] = &clientcmdapi.AuthInfo{}
	kubeconfig.Contexts["test"] = &clientcmdapi.Context{Cluster: "test", AuthInfo: "test"}
	kubeconfig.CurrentContext = "test"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kubeconfig, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}
