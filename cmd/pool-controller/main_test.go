package main

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildManagerSupportsEveryPoolKind(t *testing.T) {
	client := fake.NewClientset()
	t.Setenv("KUBE_NAMESPACE", "pools-test")
	t.Setenv("POOLS_JSON", `[{"id":"web","image":"web:1","size":2,"port":8080,"cpu":1,"memory":128}]`)
	t.Setenv("SANDBOX_POOLS_JSON", `[{"id":"python","image":"python:3.12","size":3,"port":3000,"cpu":1,"memory":256}]`)

	revisions, count, namespace, err := buildManager(client, nil, "revision")
	if err != nil {
		t.Fatalf("revision manager: %v", err)
	}
	if count != 1 || namespace != "pools-test" || revisions.Pool("web") == nil || revisions.PoolLabels("web")["pool.id"] != "web" {
		t.Fatalf("wrong revision manager: count=%d namespace=%q labels=%v", count, namespace, revisions.PoolLabels("web"))
	}

	sandboxes, count, namespace, err := buildManager(client, nil, "sandbox")
	if err != nil {
		t.Fatalf("sandbox manager: %v", err)
	}
	if count != 1 || namespace != "pools-test" || sandboxes.Pool("python") == nil || sandboxes.PoolLabels("python")["sandbox.pool"] != "python" {
		t.Fatalf("wrong sandbox manager: count=%d namespace=%q labels=%v", count, namespace, sandboxes.PoolLabels("python"))
	}
}

func TestBuildManagerRejectsUnknownKind(t *testing.T) {
	if _, _, _, err := buildManager(fake.NewClientset(), nil, "job"); err == nil {
		t.Fatal("expected unknown pool kind to fail")
	}
}
