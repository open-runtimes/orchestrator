package kubernetes

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"orchestrator/internal/job"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const (
	stubNamespace = "jobs-stub"
	stubJobsPath  = "/apis/batch/v1/namespaces/" + stubNamespace + "/jobs"
	stubPodsPath  = "/api/v1/namespaces/" + stubNamespace + "/pods"
	stubJobID     = "job-1"

	// A synthetic burst: many times client-go's default burst of 10, yet
	// inside the service burst of 400.
	concurrentArtifactReports = 300
	clientBurstSpent          = 10
	operationDeadline         = 5 * time.Second
)

type jobOperation struct {
	name   string
	method string
	run    func(context.Context, *Orchestrator) error
}

var jobOperations = []jobOperation{
	{name: "create", method: http.MethodPost, run: func(ctx context.Context, o *Orchestrator) error { return o.Run(ctx, stubJobRequest()) }},
	{name: "cancel", method: http.MethodDelete, run: func(ctx context.Context, o *Orchestrator) error { return o.Stop(ctx, stubJobID) }},
	{name: "status", method: http.MethodGet, run: func(ctx context.Context, o *Orchestrator) error {
		_, err := o.Status(ctx, stubJobID)
		return err
	}},
}

func (operation jobOperation) targets(request *http.Request) bool {
	return strings.HasPrefix(request.URL.Path, stubJobsPath) && request.Method == operation.method
}

func TestOrchestrator_IsNotStarvedByArtifactReports(t *testing.T) {
	for _, operation := range jobOperations {
		t.Run(operation.name, func(t *testing.T) {
			t.Setenv("KUBE_REQUEST_TIMEOUT", "")
			var served atomic.Int64
			o := newStubbedOrchestrator(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if strings.HasPrefix(request.URL.Path, stubJobsPath) {
					served.Add(1)
				}
				serveStub(writer, request)
			}))

			reportContext, cancelReports := context.WithCancel(context.Background())
			var started, finished sync.WaitGroup
			started.Add(concurrentArtifactReports)
			finished.Add(concurrentArtifactReports)
			for range concurrentArtifactReports {
				go func() {
					defer finished.Done()
					started.Done()
					_, _ = o.Callback(reportContext, stubJobID)
				}()
			}
			t.Cleanup(func() {
				cancelReports()
				finished.Wait()
			})
			started.Wait()

			burstDeadline := time.Now().Add(2 * time.Second)
			for served.Load() < clientBurstSpent {
				if time.Now().After(burstDeadline) {
					t.Fatalf("stub served %d artifact report lookups, want at least %d before %s", served.Load(), clientBurstSpent, operation.name)
				}
				time.Sleep(5 * time.Millisecond)
			}

			ctx, cancel := context.WithTimeout(context.Background(), operationDeadline)
			defer cancel()
			began := time.Now()
			if err := operation.run(ctx, o); err != nil {
				t.Fatalf("%s behind %d concurrent artifact reports failed after %s, want it to complete within %s: %v",
					operation.name, concurrentArtifactReports, time.Since(began), operationDeadline, err)
			}
		})
	}
}

func newStubbedOrchestrator(t *testing.T, handler http.Handler) *Orchestrator {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["stub"] = &clientcmdapi.Cluster{
		Server:                   server.URL,
		CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
	}
	kubeconfig.AuthInfos["stub"] = &clientcmdapi.AuthInfo{}
	kubeconfig.Contexts["stub"] = &clientcmdapi.Context{Cluster: "stub", AuthInfo: "stub", Namespace: stubNamespace}
	kubeconfig.CurrentContext = "stub"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kubeconfig, path); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}

	t.Setenv("KUBECONFIG", path)
	t.Setenv("KUBE_CONTEXT", "")
	t.Setenv("KUBE_NAMESPACE", stubNamespace)
	t.Setenv("KUBE_WORKLOAD_PIN_NODES", "")
	t.Setenv("KUBE_CLIENT_QPS", "")
	t.Setenv("KUBE_CLIENT_BURST", "")

	config, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	o, err := NewOrchestrator(config, &job.CallbackEmitter{})
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() })
	return o
}

func stubJobRequest() *job.Request {
	return &job.Request{
		ID: stubJobID, Image: "alpine:latest", TimeoutSeconds: 60, Workspace: "/workspace",
		Callback: &job.Callback{URL: "http://callbacks.test/hook", Key: "job-key", Events: []string{"orchestrator.job.artifact"}},
	}
}

func serveStub(writer http.ResponseWriter, request *http.Request) {
	switch {
	case strings.HasPrefix(request.URL.Path, stubJobsPath) && request.Method == http.MethodPost:
		writeStubJob(writer, http.StatusCreated)
	case strings.HasPrefix(request.URL.Path, stubJobsPath):
		writeStubJob(writer, http.StatusOK)
	case request.URL.Path == stubPodsPath:
		writeStubPods(writer)
	default:
		http.NotFound(writer, request)
	}
}

func writeStubJob(writer http.ResponseWriter, status int) {
	writeStubObject(writer, status, stubJob())
}

func stubJob() batchv1.Job {
	return batchv1.Job{
		APIVersion: "batch/v1", Kind: "Job",
		Name: jobNameFor(stubJobID), Namespace: stubNamespace, Labels: map[string]string{LabelJobID: stubJobID},
	}
}

func writeStubPods(writer http.ResponseWriter, pods ...corev1.Pod) {
	writeStubObject(writer, http.StatusOK, corev1.PodList{APIVersion: "v1", Kind: "PodList", Items: pods})
}

func writeStubObject(writer http.ResponseWriter, status int, object any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(object)
}
