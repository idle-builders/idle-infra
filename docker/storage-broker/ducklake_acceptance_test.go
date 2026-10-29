package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealDuckLakeAgainstGovernedS3Surface(t *testing.T) {
	nodeAPIRoot := os.Getenv("DUCKDB_NODE_API_ROOT")
	if nodeAPIRoot == "" {
		t.Skip("DUCKDB_NODE_API_ROOT is not set; run scripts/test-ducklake-storage-broker.sh for the live probe")
	}
	store := newLakeMemoryStore()
	backend := newLakeMemoryBackend()
	s := lakeServerForTest(store, backend)
	endpoint := httptest.NewServer(instrumentLakeRequest(newBrokerMetrics(), http.HandlerFunc(s.handleLakeS3)))
	defer endpoint.Close()
	parsedEndpoint, err := url.Parse(endpoint.URL)
	if err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(t.TempDir(), "catalog.ducklake")
	probeScript, err := filepath.Abs("../../scripts/ducklake-storage-broker-probe.mjs")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("node", probeScript)
	command.Env = append(os.Environ(),
		"DUCKDB_NODE_API_ROOT="+nodeAPIRoot,
		"DUCKLAKE_BROKER_ENDPOINT="+parsedEndpoint.Host,
		"DUCKLAKE_BUCKET=nimmly-files",
		"DUCKLAKE_SESSION_TOKEN=reviewed-token",
		"DUCKLAKE_AUTHORITY=workspace-"+workspaceA,
		"DUCKLAKE_OTHER_AUTHORITY=workspace-"+workspaceB,
		"DUCKLAKE_DATA_PREFIX=ducklake/acceptance-test/",
		"DUCKLAKE_METADATA_PATH="+catalog,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real DuckLake probe: %v\n%s", err, output)
	}
	var result struct {
		Inserted             int      `json:"inserted"`
		Committed            int      `json:"committed"`
		Snapshots            int      `json:"snapshots"`
		Files                int      `json:"files"`
		CrossWorkspaceDenied bool     `json:"crossWorkspaceDenied"`
		Scheduled            []string `json:"scheduled"`
		Cleaned              []string `json:"cleaned"`
		ObsoleteMissing      bool     `json:"obsoleteMissing"`
		CurrentReadable      bool     `json:"currentReadable"`
		PublicUntouched      bool     `json:"publicUntouched"`
		PeerUntouched        bool     `json:"peerUntouched"`
		ListingDenied        bool     `json:"listingDenied"`
		MultipartDenied      bool     `json:"multipartDenied"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(output))), &result); err != nil {
		t.Fatalf("parse probe result: %v\n%s", err, output)
	}
	t.Logf("DuckLake cleanup trace: %s", strings.TrimSpace(string(output)))
	if result.Inserted != 25000 || result.Committed != 25001 || result.Snapshots < 5 ||
		result.Files < 1 || !result.CrossWorkspaceDenied || len(result.Scheduled) == 0 ||
		len(result.Cleaned) != len(result.Scheduled) || !result.ObsoleteMissing || !result.CurrentReadable ||
		!result.PublicUntouched || !result.PeerUntouched || !result.ListingDenied || !result.MultipartDenied {
		t.Fatalf("unexpected probe result: %#v", result)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.objects) == 0 {
		t.Fatal("DuckLake created no governed rows")
	}
	deleted := 0
	for _, object := range store.objects {
		if object.ArtifactClass != ArtifactDuckLakeData || !strings.HasPrefix(object.Key, "ducklake/acceptance-test/") {
			t.Fatalf("unexpected governed row: %#v", object)
		}
		switch object.Key {
		case "ducklake/acceptance-test/public-sentinel.bin":
			if object.Scope != TenantStorageScope() || object.State != ObjectStateActive {
				t.Fatalf("public sentinel changed: %#v", object)
			}
		case "ducklake/acceptance-test/peer-sentinel.bin":
			if object.Scope != WorkspaceStorageScope(workspaceB) || object.State != ObjectStateActive {
				t.Fatalf("peer sentinel changed: %#v", object)
			}
		default:
			if object.Scope != WorkspaceStorageScope(workspaceA) {
				t.Fatalf("DuckLake object has the wrong scope: %#v", object)
			}
		}
		if object.State == ObjectStateDeleted {
			deleted++
		}
	}
	if deleted == 0 {
		t.Fatal("DuckLake probe observed no exact-path DELETE")
	}
}
