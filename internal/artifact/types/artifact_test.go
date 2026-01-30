package types

import "testing"

func TestJobDependency(t *testing.T) {
	if JobDependency != "job" {
		t.Errorf("JobDependency = %v, want 'job'", JobDependency)
	}
}

func TestResult(t *testing.T) {
	r := &Result{Status: "success", Content: "test", Error: nil}
	if r.Status != "success" {
		t.Errorf("Status = %v, want 'success'", r.Status)
	}
}
