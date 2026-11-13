package kubernetes

import (
	"context"
	"errors"
	"net/http"
	"orchestrator/internal/apperrors"
	"orchestrator/internal/job"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestOrchestrator_ReportsAStalledAPIServerAsUnavailable(t *testing.T) {
	for _, operation := range jobOperations {
		t.Run(operation.name, func(t *testing.T) {
			t.Setenv("KUBE_REQUEST_TIMEOUT", stalledRequestTimeout)
			o := newStalledOrchestrator(t, operation.targets, nil)

			err := awaitStalledOperation(t, operation.name, func() error { return operation.run(context.Background(), o) })
			if !errors.Is(err, apperrors.ErrUnavailable) {
				t.Fatalf("%s that ran out KUBE_REQUEST_TIMEOUT = %v, want apperrors.ErrUnavailable", operation.name, err)
			}
		})
	}
}

func TestOrchestrator_ReportsAClientDisconnectAsAnotherError(t *testing.T) {
	for _, operation := range jobOperations {
		t.Run(operation.name, func(t *testing.T) {
			t.Setenv("KUBE_REQUEST_TIMEOUT", "1m")
			reached := make(chan struct{}, 1)
			o := newStalledOrchestrator(t, operation.targets, reached)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-reached
				cancel()
			}()
			err := awaitStalledOperation(t, operation.name, func() error { return operation.run(ctx, o) })
			if err == nil {
				t.Fatalf("%s whose caller went away returned nil, want an error", operation.name)
			}
			if errors.Is(err, apperrors.ErrUnavailable) {
				t.Fatalf("%s whose caller went away = %v, want an error other than apperrors.ErrUnavailable", operation.name, err)
			}
		})
	}
}

func TestOrchestrator_DoesNotCacheAStatusTheDeadlineCutShort(t *testing.T) {
	t.Setenv("KUBE_REQUEST_TIMEOUT", stalledRequestTimeout)
	var stalling atomic.Bool
	stalling.Store(true)
	var jobReads atomic.Int64
	release := make(chan struct{})
	o := newStubbedOrchestrator(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, stubJobsPath):
			jobReads.Add(1)
			writeStubJob(writer, http.StatusOK)
		case request.URL.Path == stubPodsPath && stalling.Load():
			select {
			case <-request.Context().Done():
			case <-release:
			}
		case request.URL.Path == stubPodsPath:
			writeStubPods(writer, runningWorkerPod())
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(func() { close(release) })

	if _, err := o.Status(context.Background(), stubJobID); !errors.Is(err, apperrors.ErrUnavailable) {
		t.Fatalf("Status with its pod lookup stalled past KUBE_REQUEST_TIMEOUT = %v, want apperrors.ErrUnavailable", err)
	}

	stalling.Store(false)
	status, err := o.Status(context.Background(), stubJobID)
	if err != nil {
		t.Fatalf("Status once the API server answers: %v", err)
	}
	if reads := jobReads.Load(); reads != 2 {
		t.Errorf("Status read the Job %d times across both calls, want 2: the cut-short status must not be cached", reads)
	}
	if status.State != job.StateRunning {
		t.Errorf("Status once the API server answers = %q, want %q", status.State, job.StateRunning)
	}
}

func TestOrchestrator_ListStopsAtAPodLookupItsCallerCutShort(t *testing.T) {
	release := make(chan struct{})
	o := newStubbedOrchestrator(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case stubJobsPath:
			writeStubObject(writer, http.StatusOK, batchv1.JobList{APIVersion: "batch/v1", Kind: "JobList", Items: []batchv1.Job{stubJob()}})
		case stubPodsPath:
			select {
			case <-request.Context().Done():
			case <-release:
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	statuses, err := o.List(ctx)
	if !errors.Is(err, apperrors.ErrUnavailable) {
		t.Fatalf("List whose pod lookup outlived its caller's deadline = %v (statuses %+v), want apperrors.ErrUnavailable", err, statuses)
	}
}

func TestOrchestrator_StopDropsTheCachedStatusEvenWhenItTimesOut(t *testing.T) {
	t.Setenv("KUBE_REQUEST_TIMEOUT", stalledRequestTimeout)
	var jobReads atomic.Int64
	release := make(chan struct{})
	o := newStubbedOrchestrator(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasPrefix(request.URL.Path, stubJobsPath) && request.Method == http.MethodDelete:
			select {
			case <-request.Context().Done():
			case <-release:
			}
		case strings.HasPrefix(request.URL.Path, stubJobsPath):
			jobReads.Add(1)
			serveStub(writer, request)
		default:
			serveStub(writer, request)
		}
	}))
	t.Cleanup(func() { close(release) })

	if _, err := o.Status(context.Background(), stubJobID); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if err := o.Stop(context.Background(), stubJobID); !errors.Is(err, apperrors.ErrUnavailable) {
		t.Fatalf("Stop against a stalled delete = %v, want apperrors.ErrUnavailable", err)
	}
	if _, err := o.Status(context.Background(), stubJobID); err != nil {
		t.Fatalf("Status after Stop: %v", err)
	}
	if reads := jobReads.Load(); reads != 2 {
		t.Errorf("Status read the Job %d times around a Stop whose outcome is unknown, want 2: Stop must drop the cached status", reads)
	}
}

func runningWorkerPod() corev1.Pod {
	return corev1.Pod{
		Name: jobNameFor(stubJobID), Namespace: stubNamespace, Labels: map[string]string{LabelJobID: stubJobID},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  ContainerWorker,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
}
