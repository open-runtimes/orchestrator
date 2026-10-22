package kube

import "testing"

// $(HOST_IP) in the endpoint expands only from a variable defined before it.
func TestNewMountingDefinesHostIPFirst(t *testing.T) {
	env := NewMounting(9878, "t").SidecarEnv
	if env[0].Name != "HOST_IP" || env[0].ValueFrom.FieldRef.FieldPath != "status.hostIP" || env[1].Value != "http://$(HOST_IP):9878" {
		t.Fatalf("env = %+v", env)
	}
}
