package source

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
)

// Tenant deploys run with HELM_DRIVER=configmap, so release history lives in
// ConfigMaps rather than Secrets — which is what lets the console read
// deployment history with no Secrets access at all.
const helmOwnerLabel = "owner=helm"

// helmMaxPayload bounds the decompressed release document: releases embed the
// rendered manifests, and an unbounded gzip read is a decompression bomb.
const helmMaxPayload = 8 << 20

// Release is the subset of a Helm release the console needs.
type Release struct {
	Name         string
	Revision     int
	Status       string
	ChartVersion string
	AppVersion   string
	UpdatedAt    time.Time
	// Tenant CI sets image.tag to the commit SHA, which joins a revision to a
	// GitHub Actions run.
	ImageTag string
}

// releaseDocument is the narrow subset read from Helm's stored release JSON:
// decoding into it keeps the rendered manifests out of memory and responses.
type releaseDocument struct {
	Name string `json:"name"`
	Info struct {
		LastDeployed string `json:"last_deployed"`
		Status       string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
	Config  map[string]any `json:"config"`
	Version int            `json:"version"`
}

// HelmRevisions lists a namespace's raw Helm revisions, newest first. Roughly
// the last 10 survive (Helm's --history-max default), so this is recent, not
// complete. ReleaseHistory maps these onto the API shape.
func (k *K8s) HelmRevisions(ctx context.Context, namespace string) ([]Release, error) {
	list, err := k.cs.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: helmOwnerLabel,
	})
	if err != nil {
		return nil, fmt.Errorf("list helm releases in %s: %w", namespace, err)
	}

	out := make([]Release, 0, len(list.Items))
	for _, cm := range list.Items {
		payload, ok := cm.Data["release"]
		if !ok {
			if raw, ok := cm.BinaryData["release"]; ok {
				payload = string(raw)
			} else {
				continue
			}
		}

		doc, err := decodeRelease(payload)
		if err != nil {
			// One unreadable revision must not blank the history. Skip it and
			// keep the revisions that do decode.
			continue
		}

		rel := Release{
			Name:         doc.Name,
			Revision:     doc.Version,
			Status:       doc.Info.Status,
			ChartVersion: doc.Chart.Metadata.Version,
			AppVersion:   doc.Chart.Metadata.AppVersion,
			ImageTag:     imageTagFromConfig(doc.Config),
		}
		if rel.Name == "" {
			rel.Name = cm.Labels["name"]
		}
		if rel.Revision == 0 {
			rel.Revision = revisionFromLabels(cm.Labels, cm.Name)
		}
		if rel.Status == "" {
			rel.Status = cm.Labels["status"]
		}
		if t, err := time.Parse(time.RFC3339, doc.Info.LastDeployed); err == nil {
			rel.UpdatedAt = t
		} else if !cm.CreationTimestamp.IsZero() {
			rel.UpdatedAt = cm.CreationTimestamp.Time
		}

		out = append(out, rel)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Revision > out[j].Revision })
	return out, nil
}

// decodeRelease unwraps Helm's storage encoding: base64 of gzip of JSON. Both
// the base64 and gzip layers are optional in older releases.
func decodeRelease(payload string) (*releaseDocument, error) {
	raw := []byte(strings.TrimSpace(payload))

	if decoded, err := base64.StdEncoding.DecodeString(string(raw)); err == nil {
		raw = decoded
	}

	// gzip magic number.
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, err := gzip.NewReader(strings.NewReader(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer func() { _ = zr.Close() }()

		inflated, err := io.ReadAll(io.LimitReader(zr, helmMaxPayload))
		if err != nil {
			return nil, fmt.Errorf("inflate: %w", err)
		}
		raw = inflated
	}

	var doc releaseDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode release json: %w", err)
	}
	return &doc, nil
}

// imageTagFromConfig pulls image.tag out of a release's values — the join key
// to a workflow run.
func imageTagFromConfig(config map[string]any) string {
	image, ok := config["image"].(map[string]any)
	if !ok {
		return ""
	}
	tag, _ := image["tag"].(string)
	return tag
}

// revisionFromLabels recovers the revision when the payload did not carry one,
// preferring Helm's `version` label and falling back to the name suffix of
// sh.helm.release.v1.<release>.v<revision>.
func revisionFromLabels(labels map[string]string, name string) int {
	if v, err := strconv.Atoi(labels["version"]); err == nil {
		return v
	}
	if idx := strings.LastIndex(name, ".v"); idx >= 0 {
		if v, err := strconv.Atoi(name[idx+2:]); err == nil {
			return v
		}
	}
	return 0
}

// Releases maps Helm revisions onto the API shape. A release is a Helm
// revision, not a Kubernetes Deployment — the workloads reader covers those. Actor and duration stay
// nil here — they come from GitHub — and Source reports `helm` so a caller can
// tell "not enriched" from "enrichment failed".
func (k *K8s) ReleaseHistory(ctx context.Context, namespace string, limit int) ([]api.Release, error) {
	releases, err := k.HelmRevisions(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(releases) > limit {
		releases = releases[:limit]
	}

	out := make([]api.Release, 0, len(releases))
	for _, r := range releases {
		d := api.Release{
			Revision: r.Revision,
			Status:   api.ReleaseStatus(helmStatus(r.Status)),
			Source:   api.ReleaseSource("helm"),
		}
		// Absent rather than the zero time: "0001-01-01" on a deployment row
		// reads as data, where a missing field reads as unknown.
		if !r.UpdatedAt.IsZero() {
			d.UpdatedAt = ptr(r.UpdatedAt)
		}
		if r.ChartVersion != "" {
			d.ChartVersion = ptr(r.ChartVersion)
		}
		if r.AppVersion != "" {
			d.AppVersion = ptr(r.AppVersion)
		}
		// The image tag IS the commit SHA, so surface it before GitHub is
		// consulted; enrichment adds actor and duration on top.
		if r.ImageTag != "" {
			d.Commit = ptr(r.ImageTag)
		}
		out = append(out, d)
	}
	return out, nil
}

// helmStatus maps Helm's vocabulary onto the spec's enum; anything unknown
// becomes "unknown" so the API never emits an undeclared value.
func helmStatus(s string) string {
	switch strings.ToLower(s) {
	case "deployed":
		return "deployed"
	case "superseded":
		return "superseded"
	case "failed":
		return "failed"
	case "pending-install", "pending-upgrade", "pending-rollback":
		return "pending"
	case "uninstalling", "uninstalled":
		return "superseded"
	default:
		return "unknown"
	}
}
