package source

import (
	"testing"
)

func TestReplicaStatusUsesDesiredSpecWhenNoPodsExist(t *testing.T) {
	two := int32(2)
	zero := int32(0)
	tests := []struct {
		name      string
		replicas  *int32
		want      int
		wantPhase string
	}{
		{name: "configured", replicas: &two, want: 2, wantPhase: "Unavailable"},
		{name: "kubernetes default", replicas: nil, want: 1, wantPhase: "Unavailable"},
		{name: "scaled down", replicas: &zero, want: 0, wantPhase: "Scaled to zero"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, phase := replicaStatus(0, test.replicas)
			if got != test.want || phase != test.wantPhase {
				t.Errorf("replicaStatus = (%d, %q), want (%d, %q)", got, phase, test.want, test.wantPhase)
			}
		})
	}
}
