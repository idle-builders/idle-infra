// MCP tests dispatching into the real API handler over real services.
//
// The point of the MCP adapter is that a tool calls the same endpoint the
// console does. Testing it against a stub backend would prove only that the
// adapter can call a stub; here it calls the real thing and gets real cluster
// and database data back.
package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	_ "github.com/jackc/pgx/v5/stdlib"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
	"github.com/idle-builders/idle-infra/infra-console/internal/httpapi"
	"github.com/idle-builders/idle-infra/infra-console/internal/source"
)

func realBackend(t *testing.T) (http.Handler, string) {
	t.Helper()
	ns := os.Getenv("INTEG_NAMESPACE")
	if ns == "" || os.Getenv("INTEG_KUBE") == "" {
		t.Skip("integration environment not set; run integration/up.sh first")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatalf("kube config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}

	srv := &httpapi.Server{
		K8s:   source.NewK8sWithClients(cs, nil),
		Cache: source.NewCache(),
		TTL:   httpapi.TTLs{Kubernetes: time.Minute, Postgres: time.Minute},
	}
	if dsn := os.Getenv("INTEG_POSTGRES_DSN"); dsn != "" {
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatalf("postgres: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		srv.Postgres = source.NewPostgresClient(db, 100, 10*time.Second)
	}
	return api.HandlerFromMuxWithBaseURL(srv, http.NewServeMux(), "/v1"), ns
}

func session(t *testing.T, backend http.Handler) *mcpsdk.ClientSession {
	t.Helper()
	srv := NewServer(backend, "/v1", "integration")

	clientT, serverT := mcpsdk.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "integration", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcpsdk.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	text, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("%s returned %T, want TextContent", tool, res.Content[0])
	}
	return text.Text, res.IsError
}

// The MCP surface must equal the contract's agent variant, generated rather
// than hand-listed.
func TestRealToolListMatchesTheContract(t *testing.T) {
	backend, _ := realBackend(t)
	cs := session(t, backend)

	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) != len(Tools) {
		t.Fatalf("advertised %d tools, generated %d", len(listed.Tools), len(Tools))
	}
}

// An agent calling a tool must get the same real data the console renders.
func TestRealToolsReturnLiveData(t *testing.T) {
	backend, ns := realBackend(t)
	cs := session(t, backend)

	t.Run("projects", func(t *testing.T) {
		text, isErr := call(t, cs, "infra_list_projects", map[string]any{})
		if isErr {
			t.Fatalf("tool error: %s", text)
		}
		if !strings.Contains(text, ns) {
			t.Fatalf("the seeded namespace is missing from live output: %s", text)
		}
	})

	t.Run("database summary", func(t *testing.T) {
		text, isErr := call(t, cs, "infra_database_summary", map[string]any{"project": ns})
		if isErr {
			t.Fatalf("tool error: %s", text)
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(text), &d); err != nil {
			t.Fatalf("tool returned non-JSON: %s", text)
		}
		if d["schema"] != ns {
			t.Errorf("schema = %v, want %q", d["schema"], ns)
		}
		if d["tableCount"] == nil {
			t.Error("tableCount should come from the real catalog")
		}
	})

	t.Run("releases", func(t *testing.T) {
		text, isErr := call(t, cs, "infra_list_releases", map[string]any{"project": ns, "limit": 5})
		if isErr {
			t.Fatalf("tool error: %s", text)
		}
		// Decoded from the real Helm release ConfigMap.
		if !strings.Contains(text, os.Getenv("INTEG_SEED_SHA")) {
			t.Errorf("expected the seeded commit in live output: %s", text)
		}
	})

	t.Run("rows are masked for agents too", func(t *testing.T) {
		text, isErr := call(t, cs, "infra_browse_rows", map[string]any{"project": ns, "table": "players"})
		if isErr {
			t.Fatalf("tool error: %s", text)
		}
		// accounts is the table whose owner withheld columns; an agent is the
		// caller most likely to page a whole table into a context window.
		text, isErr = call(t, cs, "infra_browse_rows", map[string]any{"project": ns, "table": "accounts"})
		if isErr {
			t.Fatalf("tool error: %s", text)
		}
		for _, secret := range []string{"sk_live_DEADBEEF", "123-45-6789"} {
			if strings.Contains(text, secret) {
				t.Fatalf("a withheld value reached an agent: %s", text)
			}
		}
		if !strings.Contains(text, "columns") {
			t.Errorf("expected column metadata: %s", text)
		}
	})
}

// A 404 is an answer the model should read; a 5xx is a tool error.
func TestRealToolErrorsDistinguishClientAndServer(t *testing.T) {
	backend, _ := realBackend(t)
	cs := session(t, backend)

	text, isErr := call(t, cs, "infra_project_overview", map[string]any{"project": "definitely-not-a-tenant"})
	if isErr {
		t.Errorf("a 404 should not be a tool error: %s", text)
	}
	if !strings.Contains(text, "404") {
		t.Errorf("the status should be visible to the model: %s", text)
	}
}
