package source

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
)

// BrokerPolicyConfigMap is where Terraform writes the storage broker's grant
// policy (modules/storage-broker). Rendering it is the only place that policy
// is legible as data rather than as HCL.
const (
	BrokerPolicyConfigMap = "storage-broker-policy"
	BrokerPolicyKey       = "policy.json"
	BackupCronJob         = "pg-backup"
)

// brokerPolicy mirrors docker/storage-broker/policy.go. Kept narrow: the
// console renders the grants and nothing else.
type brokerPolicy struct {
	DefaultTTLSeconds struct {
		Get    int `json:"get"`
		Put    int `json:"put"`
		Delete int `json:"delete"`
	} `json:"defaultTTLSeconds"`
	Namespaces map[string]struct {
		Bucket           string   `json:"bucket"`
		Ops              []string `json:"ops"`
		RequireUserToken bool     `json:"requireUserToken"`
		Prefixes         []string `json:"prefixes"`
	} `json:"namespaces"`
}

// BucketFor resolves the bucket a namespace's objects live in, from the
// broker's policy ConfigMap — the same mapping the broker enforces when it
// signs URLs. Never derived from the namespace: real buckets are "<app>-files",
// so a derived name would 404 or, worse, report another tenant's usage.
func (k *K8s) BucketFor(ctx context.Context, namespace string) (string, error) {
	policy, err := k.brokerPolicy(ctx)
	if err != nil {
		return "", err
	}
	entry, ok := policy.Namespaces[namespace]
	if !ok || entry.Bucket == "" {
		return "", fmt.Errorf("no bucket mapped for namespace %q in the broker policy", namespace)
	}
	return entry.Bucket, nil
}

// brokerPolicy reads and decodes the broker's policy ConfigMap.
func (k *K8s) brokerPolicy(ctx context.Context) (brokerPolicy, error) {
	var policy brokerPolicy

	cm, err := k.cs.CoreV1().ConfigMaps(InfraNamespace).
		Get(ctx, BrokerPolicyConfigMap, metav1.GetOptions{})
	if err != nil {
		return policy, fmt.Errorf("read broker policy: %w", err)
	}
	raw, ok := cm.Data[BrokerPolicyKey]
	if !ok {
		return policy, fmt.Errorf("broker policy has no %s key", BrokerPolicyKey)
	}
	if err := json.Unmarshal([]byte(raw), &policy); err != nil {
		return policy, fmt.Errorf("decode broker policy: %w", err)
	}
	return policy, nil
}

// StoragePolicy renders a namespace's storage grants.
func (k *K8s) StoragePolicy(ctx context.Context, namespace string) (api.StoragePolicy, error) {
	policy, err := k.brokerPolicy(ctx)
	if err != nil {
		return api.StoragePolicy{}, err
	}

	entry, ok := policy.Namespaces[namespace]
	if !ok {
		return api.StoragePolicy{}, fmt.Errorf("no storage grants for namespace %q", namespace)
	}

	out := api.StoragePolicy{
		Namespace: namespace,
		Bucket:    entry.Bucket,
		Grants:    []storageGrant{},
	}

	// The TTL a grant actually gets is the per-op default; report the longest
	// of the operations the grant allows, since that is the exposure window.
	ttl := 0
	for _, op := range entry.Ops {
		switch op {
		case "get":
			ttl = max(ttl, policy.DefaultTTLSeconds.Get)
		case "put":
			ttl = max(ttl, policy.DefaultTTLSeconds.Put)
		case "delete":
			ttl = max(ttl, policy.DefaultTTLSeconds.Delete)
		}
	}

	ops := make([]api.StoragePolicyGrantsOperations, 0, len(entry.Ops))
	for _, op := range entry.Ops {
		ops = append(ops, api.StoragePolicyGrantsOperations(op))
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i] < ops[j] })

	for _, prefix := range entry.Prefixes {
		grant := storageGrant{
			Prefix:     prefix,
			Operations: ops,
			TtlSeconds: ttl,
		}
		// A ${sub} placeholder means the grant is scoped by the verified
		// Zitadel subject rather than granted to the app as a whole.
		requiresUser := entry.RequireUserToken || strings.Contains(prefix, "${sub}")
		grant.RequiresUser = &requiresUser
		out.Grants = append(out.Grants, grant)
	}

	sort.Slice(out.Grants, func(i, j int) bool { return out.Grants[i].Prefix < out.Grants[j].Prefix })
	return out, nil
}

// storageGrant aliases the anonymous struct the generator emits for
// StoragePolicy.Grants.
type storageGrant = struct {
	Operations   []api.StoragePolicyGrantsOperations `json:"operations"`
	Prefix       string                              `json:"prefix"`
	RequiresUser *bool                               `json:"requiresUser,omitempty"`
	TtlSeconds   int                                 `json:"ttlSeconds"`
}

// BackupStatus reports the PostgreSQL backup CronJob's health.
//
// restoreVerifiedAt is deliberately always nil: nothing in the platform records
// a verified restore today, and reporting a timestamp we do not have would be
// worse than reporting the gap.
func (k *K8s) BackupStatus(ctx context.Context) (api.BackupStatus, error) {
	cron, err := k.cs.BatchV1().CronJobs(InfraNamespace).Get(ctx, BackupCronJob, metav1.GetOptions{})
	if err != nil {
		return api.BackupStatus{}, fmt.Errorf("read backup cronjob: %w", err)
	}

	out := api.BackupStatus{Schedule: cron.Spec.Schedule}
	if cron.Status.LastSuccessfulTime != nil {
		t := cron.Status.LastSuccessfulTime.Time
		out.LastSuccessAt = &t
	}

	// Fall back to the newest succeeded Job when the CronJob's own status has
	// been truncated by history limits.
	if out.LastSuccessAt == nil {
		if jobs, err := k.cs.BatchV1().Jobs(InfraNamespace).List(ctx, metav1.ListOptions{}); err == nil {
			var newest *time.Time
			for i := range jobs.Items {
				job := jobs.Items[i]
				if job.Status.Succeeded == 0 || job.Status.CompletionTime == nil {
					continue
				}
				if !ownedBy(job.OwnerReferences, BackupCronJob) {
					continue
				}
				t := job.Status.CompletionTime.Time
				if newest == nil || t.After(*newest) {
					newest = &t
				}
			}
			out.LastSuccessAt = newest
		}
	}

	return out, nil
}

func ownedBy(refs []metav1.OwnerReference, name string) bool {
	for _, ref := range refs {
		if ref.Name == name {
			return true
		}
	}
	return false
}
