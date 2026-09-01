// Command deployment-chain-bench measures the controller handoffs from an
// apps/v1 Deployment/ReplicaSet, direct Pod, or orchestrator Revision create
// to Pod watch events.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	revisionapi "orchestrator/internal/revision"
	"slices"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	labelRun = "benchmark.orchestrator/run"
	labelID  = "benchmark.orchestrator/id"
)

type sample struct {
	start, acknowledged, replicaSet, pod, scheduled, ready time.Time
}

type recorder struct {
	mu      sync.Mutex
	samples map[string]*sample
}

// main keeps the benchmark phases together so their shared timing state stays
// explicit; it is a diagnostic executable rather than application logic.
//
//nolint:maintidx,nestif
func main() {
	count := flag.Int("count", 100, "number of one-replica objects")
	concurrency := flag.Int("concurrency", 25, "maximum concurrent API creates")
	mode := flag.String("mode", "deployment", "operation: deployment, replicaset, revision, revision-scale, or pod")
	baseline := flag.Int("baseline", 0, "settled zero-replica objects to create before the measured burst")
	baselineCooldown := flag.Duration("baseline-cooldown", 30*time.Second, "quiet time after the baseline reports fully observed")
	namespacePerObject := flag.Bool("namespace-per-object", false, "pre-create one isolated namespace per measured object")
	existingNamespace := flag.String("namespace", "", "use an existing namespace instead of creating a disposable one")
	namespaceCooldown := flag.Duration("namespace-cooldown", 10*time.Second, "quiet time after per-object namespaces are created")
	kubeContext := flag.String("context", "", "kubeconfig context (empty uses current context)")
	timeout := flag.Duration("timeout", 90*time.Second, "maximum time to wait for Pods")
	qps := flag.Float64("qps", 500, "benchmark client QPS limit")
	burst := flag.Int("burst", 1000, "benchmark client burst limit")
	waitReady := flag.Bool("wait-ready", false, "wait for every Pod to become ready")
	keep := flag.Bool("keep", false, "keep the benchmark namespace")
	flag.Parse()
	if *count < 1 || *concurrency < 1 {
		log.Fatal("count and concurrency must be positive")
	}
	if *baseline < 0 {
		log.Fatal("baseline must not be negative")
	}
	if *namespacePerObject && *baseline > 0 {
		log.Fatal("baseline and namespace-per-object cannot be combined")
	}
	if *namespacePerObject && (*mode == "revision" || *mode == "revision-scale") {
		log.Fatal("revision mode requires the controller's single benchmark namespace")
	}
	if *namespacePerObject && *existingNamespace != "" {
		log.Fatal("namespace and namespace-per-object cannot be combined")
	}
	if *mode != "deployment" && *mode != "replicaset" && *mode != "revision" && *mode != "revision-scale" && *mode != "pod" {
		log.Fatal("mode must be deployment, replicaset, revision, revision-scale, or pod")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: *kubeContext}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		log.Fatal(err)
	}
	config.QPS = float32(*qps)
	config.Burst = *burst
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}

	run := fmt.Sprintf("r%x", time.Now().UnixNano())
	namespace := "deployment-chain-" + run
	createdNamespaces := *existingNamespace == ""
	if *existingNamespace != "" {
		namespace = *existingNamespace
	}
	namespaces := []string{namespace}
	if *namespacePerObject {
		namespaces = make([]string, *count)
		for i := range *count {
			namespaces[i] = fmt.Sprintf("%s-%05d", namespace, i)
		}
	}
	if createdNamespaces {
		if err := createNamespaces(ctx, client, namespaces, run, *concurrency); err != nil {
			log.Fatal(err)
		}
	}
	if *namespacePerObject {
		log.Printf("created %d benchmark namespaces; cooling down for %s", len(namespaces), *namespaceCooldown)
		time.Sleep(*namespaceCooldown)
	}
	if !*keep && createdNamespaces {
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			for _, name := range namespaces {
				if err := client.CoreV1().Namespaces().Delete(cleanupCtx, name, metav1.DeleteOptions{}); err != nil {
					log.Printf("cleanup namespace %s: %v", name, err)
				}
			}
		}()
	}
	if *baseline > 0 {
		baselineRun := run + "-base"
		started := time.Now()
		if err := createBaseline(ctx, client, dynamicClient, *mode, namespace, baselineRun, *baseline, *concurrency); err != nil {
			log.Fatal(err)
		}
		for ctx.Err() == nil {
			if *mode == "revision" || *mode == "revision-scale" {
				revisions, err := dynamicClient.Resource(revisionapi.Resource()).Namespace(namespace).List(ctx, metav1.ListOptions{
					LabelSelector: labelRun + "=" + baselineRun,
				})
				if err != nil {
					log.Fatal(err)
				}
				observed := 0
				for i := range revisions.Items {
					generation := revisions.Items[i].GetGeneration()
					statusGeneration, _, _ := unstructured.NestedInt64(revisions.Items[i].Object, "status", "observedGeneration")
					if statusGeneration >= generation {
						observed++
					}
				}
				if len(revisions.Items) == *baseline && observed == *baseline {
					break
				}
				log.Printf("baseline settling: %d/%d Revisions, %d/%d observed", len(revisions.Items), *baseline, observed, *baseline)
				time.Sleep(time.Second)
				continue
			}
			replicaSets, err := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labelRun + "=" + baselineRun,
			})
			if err != nil {
				log.Fatal(err)
			}
			deployments, err := client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labelRun + "=" + baselineRun,
			})
			if err != nil {
				log.Fatal(err)
			}
			observed := 0
			for i := range deployments.Items {
				if deployments.Items[i].Status.ObservedGeneration >= deployments.Items[i].Generation {
					observed++
				}
			}
			if len(replicaSets.Items) == *baseline && observed == *baseline {
				break
			}
			log.Printf("baseline settling: %d/%d ReplicaSets, %d/%d Deployments observed",
				len(replicaSets.Items), *baseline, observed, *baseline)
			time.Sleep(time.Second)
		}
		if ctx.Err() != nil {
			log.Fatalf("baseline did not settle: %v", ctx.Err())
		}
		log.Printf("baseline settled: %d %ss in %s; cooling down for %s",
			*baseline, *mode, time.Since(started).Round(time.Millisecond), *baselineCooldown)
		// Let status writes and watch events from the last baseline objects drain
		// before starting the measured watches.
		time.Sleep(*baselineCooldown)
	}
	if *mode == "revision-scale" {
		started := time.Now()
		if err := createRevisions(ctx, dynamicClient, namespace, run, *count, *concurrency, 0, "d-"); err != nil {
			log.Fatal(err)
		}
		if err := waitRevisionsObserved(ctx, dynamicClient, namespace, run, *count); err != nil {
			log.Fatal(err)
		}
		log.Printf("prepared %d zero-replica Revisions in %s", *count, time.Since(started).Round(time.Millisecond))
	}

	selector := labelRun + "=" + run
	watchNamespace := namespace
	if *namespacePerObject {
		watchNamespace = metav1.NamespaceAll
	}
	rsWatch, err := client.AppsV1().ReplicaSets(watchNamespace).Watch(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		log.Fatal(err)
	}
	defer rsWatch.Stop()
	podWatch, err := client.CoreV1().Pods(watchNamespace).Watch(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		log.Fatal(err)
	}
	defer podWatch.Stop()

	rec := &recorder{samples: make(map[string]*sample, *count)}
	for i := range *count {
		rec.samples[fmt.Sprintf("d-%05d", i)] = &sample{}
	}
	go observeReplicaSets(rsWatch, rec)
	go observePods(podWatch, rec)

	burstStart := time.Now()
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup
	var createMu sync.Mutex
	var createErrors []error
	for id := range rec.samples {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			started := time.Now()
			rec.set(id, func(s *sample) { s.start = started })
			targetNamespace := namespace
			if *namespacePerObject {
				targetNamespace = fmt.Sprintf("%s-%s", namespace, id[2:])
			}
			var err error
			switch *mode {
			case "deployment":
				_, err = client.AppsV1().Deployments(targetNamespace).Create(ctx, deployment(run, id), metav1.CreateOptions{})
			case "replicaset":
				_, err = client.AppsV1().ReplicaSets(targetNamespace).Create(ctx, replicaSet(run, id), metav1.CreateOptions{})
			case "pod":
				_, err = client.CoreV1().Pods(targetNamespace).Create(ctx, pod(run, id), metav1.CreateOptions{})
			case "revision":
				_, err = dynamicClient.Resource(revisionapi.Resource()).Namespace(targetNamespace).Create(ctx, revision(run, id, 1), metav1.CreateOptions{})
			case "revision-scale":
				err = revisionapi.NewClient(dynamicClient).Scale(ctx, targetNamespace, "dep-"+id, 1)
			}
			acknowledged := time.Now()
			if err != nil {
				createMu.Lock()
				createErrors = append(createErrors, fmt.Errorf("%s: %w", id, err))
				createMu.Unlock()
				return
			}
			rec.set(id, func(s *sample) { s.acknowledged = acknowledged })
		})
	}
	wg.Wait()
	createFinished := time.Now()
	if len(createErrors) > 0 {
		log.Fatalf("%d create errors; first: %v", len(createErrors), createErrors[0])
	}

	var podsCompleteAt time.Time
	for ctx.Err() == nil {
		_, pods, _, ready := rec.counts()
		if pods == *count {
			if *waitReady && ready == *count {
				break
			}
			if !*waitReady {
				if podsCompleteAt.IsZero() {
					podsCompleteAt = time.Now()
				}
				if time.Since(podsCompleteAt) >= 2*time.Second {
					break
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	rs, pods, scheduled, ready := rec.counts()

	if *namespacePerObject {
		fmt.Printf("namespaces: %d under prefix %s\n", len(namespaces), namespace)
	} else {
		fmt.Printf("namespace: %s\n", namespace)
	}
	fmt.Printf("mode: %s, baseline deployments: %d, objects: %d, create concurrency: %d\n", *mode, *baseline, *count, *concurrency)
	action := "create"
	if *mode == "revision-scale" {
		action = "scale"
	}
	fmt.Printf("burst %s wall time: %s (%.1f ops/s)\n", action, createFinished.Sub(burstStart), float64(*count)/createFinished.Sub(burstStart).Seconds())
	fmt.Printf("observed: replicasets=%d pods=%d scheduled=%d ready=%d\n", rs, pods, scheduled, ready)
	printMetric("API "+action, rec.durations(func(s *sample) (time.Time, time.Time) { return s.start, s.acknowledged }))
	printMetric("ack -> ReplicaSet", rec.durations(func(s *sample) (time.Time, time.Time) { return s.acknowledged, s.replicaSet }))
	printMetric("ReplicaSet -> Pod", rec.durations(func(s *sample) (time.Time, time.Time) { return s.replicaSet, s.pod }))
	printMetric("ack -> Pod", rec.durations(func(s *sample) (time.Time, time.Time) { return s.acknowledged, s.pod }))
	printMetric("Pod -> scheduled", rec.durations(func(s *sample) (time.Time, time.Time) { return s.pod, s.scheduled }))
	printMetric("scheduled -> ready", rec.durations(func(s *sample) (time.Time, time.Time) { return s.scheduled, s.ready }))
	printMetric("ack -> ready", rec.durations(func(s *sample) (time.Time, time.Time) { return s.acknowledged, s.ready }))
	if ctx.Err() != nil {
		fmt.Printf("wait ended: %v\n", ctx.Err())
	}
}

func deployment(run, id string) *appsv1.Deployment {
	return deploymentWithReplicas(run, id, 1)
}

func createNamespaces(ctx context.Context, client kubernetes.Interface, names []string, run string, concurrency int) error {
	sem := make(chan struct{}, concurrency)
	errs := make(chan error, len(names))
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{labelRun: run}},
			}, metav1.CreateOptions{})
			if err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

func deploymentWithReplicas(run, id string, replicas int32) *appsv1.Deployment {
	labels := map[string]string{labelRun: run, labelID: id}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: id, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: template(labels),
		},
	}
}

func createBaseline(ctx context.Context, client kubernetes.Interface, dynamicClient dynamic.Interface, mode, namespace, run string, count, concurrency int) error {
	if mode == "revision" || mode == "revision-scale" {
		return createRevisions(ctx, dynamicClient, namespace, run, count, concurrency, 0, "baseline-")
	}
	sem := make(chan struct{}, concurrency)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		id := fmt.Sprintf("baseline-%05d", i)
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			var err error
			_, err = client.AppsV1().Deployments(namespace).Create(ctx, deploymentWithReplicas(run, id, 0), metav1.CreateOptions{})
			if err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

func createRevisions(ctx context.Context, client dynamic.Interface, namespace, run string, count, concurrency int, replicas int32, prefix string) error {
	sem := make(chan struct{}, concurrency)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := range count {
		id := fmt.Sprintf("%s%05d", prefix, i)
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, err := client.Resource(revisionapi.Resource()).Namespace(namespace).Create(ctx, revision(run, id, replicas), metav1.CreateOptions{}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	return nil
}

func waitRevisionsObserved(ctx context.Context, client dynamic.Interface, namespace, run string, count int) error {
	for ctx.Err() == nil {
		items, err := client.Resource(revisionapi.Resource()).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelRun + "=" + run})
		if err != nil {
			return err
		}
		observed := 0
		for i := range items.Items {
			generation := items.Items[i].GetGeneration()
			statusGeneration, _, _ := unstructured.NestedInt64(items.Items[i].Object, "status", "observedGeneration")
			if statusGeneration >= generation {
				observed++
			}
		}
		if len(items.Items) == count && observed == count {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ctx.Err()
}

func revision(run, id string, replicas int32) *unstructured.Unstructured {
	labels := map[string]string{
		labelRun:              run,
		labelID:               id,
		"managed-by":          "deployments-service",
		"deployment.id":       id,
		"deployment.revision": id,
	}
	revision := &revisionapi.Revision{
		TypeMeta:   metav1.TypeMeta{APIVersion: revisionapi.APIVersion(), Kind: revisionapi.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: "dep-" + id, Labels: labels},
		Spec: revisionapi.Spec{
			Replicas: replicas,
			Template: template(labels),
		},
	}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(revision)
	if err != nil {
		panic(err)
	}
	return &unstructured.Unstructured{Object: object}
}

func replicaSet(run, id string) *appsv1.ReplicaSet {
	replicas := int32(1)
	labels := map[string]string{labelRun: run, labelID: id}
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: id, Labels: labels},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: template(labels),
		},
	}
}

func pod(run, id string) *corev1.Pod {
	labels := map[string]string{labelRun: run, labelID: id}
	t := template(labels)
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: id, Labels: labels}, Spec: t.Spec}
}

func template(labels map[string]string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			TerminationGracePeriodSeconds: ptr(int64(0)),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr(true),
				RunAsUser:    ptr(int64(65532)),
				RunAsGroup:   ptr(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:            "pause",
				Image:           "registry.k8s.io/pause:3.10",
				ImagePullPolicy: corev1.PullIfNotPresent,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr(false),
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
}

func observeReplicaSets(w watch.Interface, rec *recorder) {
	for event := range w.ResultChan() {
		rs, ok := event.Object.(*appsv1.ReplicaSet)
		if !ok {
			continue
		}
		id := rs.Labels[labelID]
		now := time.Now()
		rec.set(id, func(s *sample) {
			if s.replicaSet.IsZero() {
				s.replicaSet = now
			}
		})
	}
}

func observePods(w watch.Interface, rec *recorder) {
	for event := range w.ResultChan() {
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		id := pod.Labels[labelID]
		now := time.Now()
		rec.set(id, func(s *sample) {
			if s.pod.IsZero() {
				s.pod = now
			}
			if s.scheduled.IsZero() && conditionTrue(pod.Status.Conditions, corev1.PodScheduled) {
				s.scheduled = now
			}
			if s.ready.IsZero() && conditionTrue(pod.Status.Conditions, corev1.PodReady) {
				s.ready = now
			}
		})
	}
}

func conditionTrue(conditions []corev1.PodCondition, kind corev1.PodConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == kind && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *recorder) set(id string, update func(*sample)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.samples[id]; s != nil {
		update(s)
	}
}

func (r *recorder) counts() (int, int, int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var replicaSets, pods, scheduled, ready int
	for _, s := range r.samples {
		if !s.replicaSet.IsZero() {
			replicaSets++
		}
		if !s.pod.IsZero() {
			pods++
		}
		if !s.scheduled.IsZero() {
			scheduled++
		}
		if !s.ready.IsZero() {
			ready++
		}
	}
	return replicaSets, pods, scheduled, ready
}

func (r *recorder) durations(selectTimes func(*sample) (time.Time, time.Time)) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := make([]time.Duration, 0, len(r.samples))
	for _, s := range r.samples {
		from, to := selectTimes(s)
		if from.IsZero() || to.IsZero() || to.Before(from) {
			continue
		}
		values = append(values, to.Sub(from))
	}
	return values
}

func printMetric(name string, values []time.Duration) {
	if len(values) == 0 {
		fmt.Printf("%-22s no samples\n", name)
		return
	}
	slices.Sort(values)
	fmt.Printf("%-22s n=%-4d p50=%-9s p90=%-9s p95=%-9s p99=%-9s max=%s\n",
		name, len(values), percentile(values, 0.50), percentile(values, 0.90),
		percentile(values, 0.95), percentile(values, 0.99), values[len(values)-1])
}

func percentile(values []time.Duration, quantile float64) time.Duration {
	index := int(float64(len(values)-1) * quantile)
	return values[index].Round(time.Millisecond)
}

func ptr[T any](value T) *T { return &value }
