package source

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
)

// TenantLabel marks a namespace as a tenant project. Set by
// terraform/main/modules/tenant. Discovering projects from this label rather
// than a hardcoded roster means the console reflects what Terraform actually
// provisioned.
const TenantLabel = "platform.infra/tenant"

// InfraNamespace holds the shared services.
const InfraNamespace = "infra"

// Helm sets these on every object it manages, which is how chart and version
// are read without touching a release Secret. The console's ClusterRole
// deliberately grants no access to Secrets.
const (
	chartLabel      = "helm.sh/chart"
	appVersionLabel = "app.kubernetes.io/version"
)

// K8s reads cluster state. Every method is read-only.
type K8s struct {
	cs      kubernetes.Interface
	metrics metricsv.Interface // nil when metrics-server is unavailable
}

// NewK8s builds a client, preferring in-cluster credentials and falling back to
// a kubeconfig so the service can run locally against a developer's context
// without being deployed.
func NewK8s() (*K8s, error) {
	cfg, err := restConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}

	k := &K8s{cs: cs}

	// metrics-server is optional. Its absence costs the instantaneous usage
	// numbers, not the whole source, so a failure here is not fatal.
	if mc, err := metricsv.NewForConfig(cfg); err == nil {
		k.metrics = mc
	}
	return k, nil
}

// NewK8sWithClients is the seam tests use.
func NewK8sWithClients(cs kubernetes.Interface, metrics metricsv.Interface) *K8s {
	return &K8s{cs: cs, metrics: metrics}
}

func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config and no home directory for kubeconfig: %w", err)
		}
		path = filepath.Join(home, ".kube", "config")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	return cfg, nil
}

// Projects lists tenant namespaces with a health roll-up.
func (k *K8s) Projects(ctx context.Context) ([]api.ProjectSummary, error) {
	nsList, err := k.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: TenantLabel + "=true",
	})
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}

	out := make([]api.ProjectSummary, 0, len(nsList.Items))
	for _, ns := range nsList.Items {
		summary := api.ProjectSummary{
			Name:      ns.Name,
			Namespace: ns.Name,
			Schema:    ptr(ns.Name), // schema-per-app: schema name matches the tenant
			Health:    api.ProjectSummaryHealth("unknown"),
		}

		pods, err := k.cs.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			// One unreadable namespace must not blank the whole project list;
			// it reports unknown health and the rest still render.
			out = append(out, summary)
			continue
		}

		ready, desired := podReadiness(pods.Items)
		summary.PodsReady = ptr(ready)
		summary.PodsDesired = ptr(desired)
		summary.Health = api.ProjectSummaryHealth(health(ready, desired))
		out = append(out, summary)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// podReadiness counts ready containers against desired pods, ignoring pods that
// have already completed — a finished migration Job is not a health problem.
func podReadiness(pods []corev1.Pod) (ready, desired int) {
	for _, p := range pods {
		if p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		desired++
		if isPodReady(p) {
			ready++
		}
	}
	return ready, desired
}

func isPodReady(p corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func health(ready, desired int) string {
	switch {
	case desired == 0:
		return "unknown"
	case ready == 0:
		return "down"
	case ready < desired:
		return "degraded"
	default:
		return "ok"
	}
}

// Nodes lists worker nodes, enriched with instantaneous usage when
// metrics-server answers.
func (k *K8s) Nodes(ctx context.Context) ([]api.Node, error) {
	list, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	podsPerNode := k.podsPerNode(ctx)
	usage := k.nodeUsage(ctx)

	out := make([]api.Node, 0, len(list.Items))
	for _, n := range list.Items {
		node := api.Node{
			Name:           n.Name,
			KubeletVersion: n.Status.NodeInfo.KubeletVersion,
			Ready:          nodeReady(n),
		}
		if conditions := abnormalConditions(n); len(conditions) > 0 {
			node.Conditions = &conditions
		}
		if count, ok := podsPerNode[n.Name]; ok {
			node.Pods = ptr(count)
		}
		if u, ok := usage[n.Name]; ok {
			if cpu := ratio(u.cpuMilli, n.Status.Allocatable.Cpu().MilliValue()); cpu != nil {
				node.CpuUsedRatio = cpu
			}
			if mem := ratio(u.memBytes, n.Status.Allocatable.Memory().Value()); mem != nil {
				node.MemoryUsedRatio = mem
			}
		}
		out = append(out, node)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// abnormalConditions reports pressure conditions that are currently true.
// NodeReady is excluded — it has its own field, and listing "Ready" as an
// abnormal condition would read as a problem.
func abnormalConditions(n corev1.Node) []string {
	var out []string
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			continue
		}
		if c.Status == corev1.ConditionTrue {
			out = append(out, string(c.Type))
		}
	}
	sort.Strings(out)
	return out
}

type nodeUsage struct {
	cpuMilli int64
	memBytes int64
}

func (k *K8s) nodeUsage(ctx context.Context) map[string]nodeUsage {
	out := map[string]nodeUsage{}
	if k.metrics == nil {
		return out
	}
	list, err := k.metrics.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, m := range list.Items {
		out[m.Name] = nodeUsage{
			cpuMilli: m.Usage.Cpu().MilliValue(),
			memBytes: m.Usage.Memory().Value(),
		}
	}
	return out
}

func (k *K8s) podsPerNode(ctx context.Context) map[string]int {
	out := map[string]int{}
	pods, err := k.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		out[p.Spec.NodeName]++
	}
	return out
}

// Capacity reports cluster-wide requested-versus-allocatable ratios.
//
// These are *requests*, not usage: they answer "can another pod be scheduled",
// which is the question a capacity panel is for. Source is always
func (k *K8s) Capacity(ctx context.Context) (api.Capacity, error) {
	nodes, err := k.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return api.Capacity{}, fmt.Errorf("list nodes: %w", err)
	}
	pods, err := k.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return api.Capacity{}, fmt.Errorf("list pods: %w", err)
	}

	var allocCPU, allocMem int64
	for _, n := range nodes.Items {
		allocCPU += n.Status.Allocatable.Cpu().MilliValue()
		allocMem += n.Status.Allocatable.Memory().Value()
	}

	var reqCPU, reqMem int64
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			reqCPU += c.Resources.Requests.Cpu().MilliValue()
			reqMem += c.Resources.Requests.Memory().Value()
		}
	}

	out := api.Capacity{Source: ptr(api.CapacitySource("metrics-server"))}
	if r := ratio(reqCPU, allocCPU); r != nil {
		out.CpuRequestedRatio = *r
	}
	if r := ratio(reqMem, allocMem); r != nil {
		out.MemoryRequestedRatio = *r
	}
	if allocCPU > 0 {
		out.CpuCores = ptr(float32(allocCPU) / 1000)
	}
	if allocMem > 0 {
		out.MemoryBytes = ptr(allocMem)
	}
	return out, nil
}

// Workloads lists a namespace's Deployments, StatefulSets and CronJobs with
// live readiness.
func (k *K8s) Workloads(ctx context.Context, namespace string) ([]api.Workload, error) {
	out := []api.Workload{}

	deploys, err := k.cs.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", namespace, err)
	}
	for _, d := range deploys.Items {
		desired, phase := replicaStatus(d.Status.ReadyReplicas, d.Spec.Replicas)
		w := api.Workload{
			Name:     d.Name,
			Kind:     api.WorkloadKind("Deployment"),
			Ready:    int(d.Status.ReadyReplicas),
			Replicas: ptr(desired),
			Phase:    phase,
		}
		if img, names := containers(d.Spec.Template.Spec.Containers); img != "" {
			w.Image = ptr(img)
			w.Containers = &names
		}
		helmLabels(&w, d.Labels)
		if !d.CreationTimestamp.IsZero() {
			w.StartedAt = ptr(d.CreationTimestamp.Time)
		}
		out = append(out, w)
	}

	sets, err := k.cs.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, s := range sets.Items {
			desired, phase := replicaStatus(s.Status.ReadyReplicas, s.Spec.Replicas)
			w := api.Workload{
				Name:     s.Name,
				Kind:     api.WorkloadKind("StatefulSet"),
				Ready:    int(s.Status.ReadyReplicas),
				Replicas: ptr(desired),
				Phase:    phase,
			}
			if img, names := containers(s.Spec.Template.Spec.Containers); img != "" {
				w.Image = ptr(img)
				w.Containers = &names
			}
			helmLabels(&w, s.Labels)
			out = append(out, w)
		}
	}

	crons, err := k.cs.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, c := range crons.Items {
			w := api.Workload{
				Name:  c.Name,
				Kind:  api.WorkloadKind("CronJob"),
				Ready: len(c.Status.Active),
				Phase: "Scheduled",
			}
			if c.Spec.Suspend != nil && *c.Spec.Suspend {
				w.Phase = "Suspended"
			}
			if img, names := containers(c.Spec.JobTemplate.Spec.Template.Spec.Containers); img != "" {
				w.Image = ptr(img)
				w.Containers = &names
			}
			helmLabels(&w, c.Labels)
			out = append(out, w)
		}
	}

	// Pods, attached to the controller that owns them. ready/replicas says how
	// many there SHOULD be; this says which actually exist — the difference is
	// the whole question when something is not coming up.
	if pods, err := k.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		restarts := map[string]int{}
		owned := map[string][]api.Pod{}
		for _, p := range pods.Items {
			owner := controllerName(p)

			var podRestarts int
			ready := false
			for _, cs := range p.Status.ContainerStatuses {
				podRestarts += int(cs.RestartCount)
				if cs.Ready {
					ready = true
				}
			}
			restarts[owner] += podRestarts

			entry := api.Pod{
				Name:  p.Name,
				Phase: string(p.Status.Phase),
				Ready: &ready,
			}
			if podRestarts > 0 {
				entry.Restarts = ptr(podRestarts)
			}
			if p.Spec.NodeName != "" {
				node := p.Spec.NodeName
				entry.Node = &node
			}
			if p.Status.StartTime != nil {
				started := p.Status.StartTime.Time
				entry.StartedAt = &started
			}
			owned[owner] = append(owned[owner], entry)
		}
		for i := range out {
			if n, ok := restarts[out[i].Name]; ok && n > 0 {
				out[i].Restarts = ptr(n)
			}
			if list, ok := owned[out[i].Name]; ok {
				out[i].Pods = &list
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// controllerName walks a pod name back to its controller by trimming the
// ReplicaSet and pod hash suffixes. Cheaper than resolving ownerReferences
// through ReplicaSets, and good enough for aggregating restart counts.
func controllerName(p corev1.Pod) string {
	for _, ref := range p.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		switch ref.Kind {
		case "StatefulSet", "DaemonSet":
			return ref.Name
		case "ReplicaSet":
			if idx := strings.LastIndex(ref.Name, "-"); idx > 0 {
				return ref.Name[:idx]
			}
			return ref.Name
		default:
			return ref.Name
		}
	}
	return p.Name
}

func containers(cs []corev1.Container) (image string, names []string) {
	for _, c := range cs {
		names = append(names, c.Name)
		if image == "" {
			image = c.Image
		}
	}
	return image, names
}

func deploymentPhase(ready, desired int) string {
	switch {
	case desired == 0:
		return "Scaled to zero"
	case ready == 0:
		return "Unavailable"
	case ready < desired:
		return "Progressing"
	default:
		return "Running"
	}
}

func desiredReplicas(replicas *int32) int {
	if replicas == nil {
		return 1
	}
	return int(*replicas)
}

func replicaStatus(ready int32, replicas *int32) (int, string) {
	desired := desiredReplicas(replicas)
	return desired, deploymentPhase(int(ready), desired)
}

// Quota reports a namespace's ResourceQuota usage. A namespace without a quota
// is not an error — it returns an empty Quota.
func (k *K8s) Quota(ctx context.Context, namespace string) (api.Quota, error) {
	list, err := k.cs.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return api.Quota{}, fmt.Errorf("list resourcequotas in %s: %w", namespace, err)
	}

	var out api.Quota
	for _, q := range list.Items {
		hardCPU, okHardCPU := q.Status.Hard[corev1.ResourceRequestsCPU]
		usedCPU, okUsedCPU := q.Status.Used[corev1.ResourceRequestsCPU]
		if okHardCPU && okUsedCPU {
			out.CpuHard = ptr(hardCPU.String())
			out.CpuUsedRatio = ratio(usedCPU.MilliValue(), hardCPU.MilliValue())
		}

		hardMem, okHardMem := q.Status.Hard[corev1.ResourceRequestsMemory]
		usedMem, okUsedMem := q.Status.Used[corev1.ResourceRequestsMemory]
		if okHardMem && okUsedMem {
			out.MemoryHard = ptr(hardMem.String())
			out.MemoryUsedRatio = ratio(usedMem.Value(), hardMem.Value())
		}

		if hardPods, ok := q.Status.Hard[corev1.ResourcePods]; ok {
			out.PodsHard = ptr(int(hardPods.Value()))
			if usedPods, ok := q.Status.Used[corev1.ResourcePods]; ok {
				out.PodsUsed = ptr(int(usedPods.Value()))
			}
		}
	}
	return out, nil
}

// helmLabels copies chart and version off a controller's Helm labels.
//
// The labels, never the release Secret: reading those would need cluster-wide
// `get secrets`, which the console's ClusterRole deliberately does not grant.
func helmLabels(w *api.Workload, labels map[string]string) {
	if chart := labels[chartLabel]; chart != "" {
		w.Chart = ptr(chart)
	}
	if version := labels[appVersionLabel]; version != "" {
		w.AppVersion = ptr(version)
	}
}

// SharedServices lists the infra namespace's Deployments.
//
// Chart and version come from Helm's labels, never from the release Secret —
// reading those would require cluster-wide `get secrets`, which the console's
// ClusterRole does not grant (AC2).
func (k *K8s) SharedServices(ctx context.Context) ([]api.SharedService, error) {
	list, err := k.cs.AppsV1().Deployments(InfraNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list deployments in %s: %w", InfraNamespace, err)
	}

	out := make([]api.SharedService, 0, len(list.Items))
	for _, d := range list.Items {
		svc := api.SharedService{
			Name:     d.Name,
			Ready:    int(d.Status.ReadyReplicas),
			Replicas: int(d.Status.Replicas),
			Owner:    api.SharedServiceOwner("opentofu"),
		}
		if chart := d.Labels[chartLabel]; chart != "" {
			svc.Chart = ptr(chart)
		}
		if version := d.Labels[appVersionLabel]; version != "" {
			svc.AppVersion = ptr(version)
		}
		out = append(out, svc)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Ingresses lists a namespace's hostnames.
//
// TlsSource is cloudflare-universal unless the Ingress declares a TLS block:
// tunnel-fronted hosts terminate TLS at the edge and have no cert-manager
// Certificate, so reporting a Kubernetes expiry for them would be fiction.
func (k *K8s) Ingresses(ctx context.Context, namespace string) ([]api.Hostname, error) {
	list, err := k.cs.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list ingresses in %s: %w", namespace, err)
	}

	out := []api.Hostname{}
	for _, ing := range list.Items {
		tlsHosts := map[string]bool{}
		for _, t := range ing.Spec.TLS {
			for _, h := range t.Hosts {
				tlsHosts[h] = true
			}
		}
		for _, rule := range ing.Spec.Rules {
			if rule.Host == "" {
				continue
			}
			h := api.Hostname{
				Host:       rule.Host,
				RecordType: "ingress",
				TlsSource:  api.HostnameTlsSource("cloudflare-universal"),
			}
			if tlsHosts[rule.Host] {
				h.TlsSource = api.HostnameTlsSource("cert-manager")
			}
			if svc := firstBackendService(rule); svc != "" {
				h.RoutesTo = ptr(ing.Namespace + "/" + svc)
			}
			out = append(out, h)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

func firstBackendService(rule networkingv1.IngressRule) string {
	if rule.HTTP == nil {
		return ""
	}
	for _, p := range rule.HTTP.Paths {
		if p.Backend.Service != nil {
			return p.Backend.Service.Name
		}
	}
	return ""
}

// LogOptions bounds a log read. There is no historical store behind this, so a
// restarted pod's previous output is gone unless Previous is set.
type LogOptions struct {
	Container string
	TailLines int
	Previous  bool
}

// Logs streams a pod's container log, bounded by TailLines.
func (k *K8s) Logs(ctx context.Context, namespace, pod string, opts LogOptions) (string, error) {
	if opts.TailLines <= 0 {
		opts.TailLines = 200
	}
	tail := int64(opts.TailLines)
	req := k.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: opts.Container,
		TailLines: &tail,
		Previous:  opts.Previous,
		// Timestamps make an otherwise unordered multi-container tail readable.
		Timestamps: true,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("stream logs for %s/%s: %w", namespace, pod, err)
	}
	defer func() { _ = stream.Close() }()

	// Hard cap the body regardless of TailLines: a single log line can be
	// arbitrarily long, and this response is buffered.
	const maxBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(stream, maxBytes))
	if err != nil {
		return "", fmt.Errorf("read logs for %s/%s: %w", namespace, pod, err)
	}
	return string(data), nil
}

// NamespaceExists reports whether a tenant namespace exists, so a request for
// an unknown project answers 404 rather than an empty aggregate.
func (k *K8s) NamespaceExists(ctx context.Context, namespace string) (bool, error) {
	ns, err := k.cs.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return ns.Labels[TenantLabel] == "true", nil
}

// WorkloadNamespaceExists gates the workloads reader, which covers one more
// namespace than the tenant surfaces do.
//
// A shared service is a workload like any other — it is simply owned by infra
// rather than by a tenant — so infra is addressable for workloads. It is NOT a
// tenant: it has no schema, bucket or Zitadel org, so database, storage, auth
// and the rest keep using NamespaceExists and still 404 here.
func (k *K8s) WorkloadNamespaceExists(ctx context.Context, namespace string) (bool, error) {
	if namespace == InfraNamespace {
		if _, err := k.cs.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err != nil {
			return false, err
		}
		return true, nil
	}
	return k.NamespaceExists(ctx, namespace)
}

// ratio returns used/total as a fraction, or nil when total is zero — an
// undefined ratio must not render as 0%.
func ratio(used, total int64) *float32 {
	if total <= 0 {
		return nil
	}
	v := float32(used) / float32(total)
	return &v
}

func ptr[T any](v T) *T { return &v }
