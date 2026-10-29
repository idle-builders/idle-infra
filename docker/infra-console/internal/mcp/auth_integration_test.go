// The MCP adapter dispatched through the AUTHENTICATED handler chain.
//
// This file exists because of a defect the rest of the suite could not see.
// The other MCP tests pass the raw API handler, but production wires
// mcp.Handler over the *protected* handler (main.go). The adapter dispatches a
// tool by synthesizing an HTTP request, and a synthesized request carries no
// Authorization header and no cookie — so the inner middleware rejected every
// tool call with 401 the moment OIDC_ISSUER was set, which Terraform always
// sets. The MCP surface was completely non-functional in any deployment and
// every test still passed.
//
// These tests therefore assemble the chain exactly as main.go does and drive
// it over real HTTP, against real Zitadel.
package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/idle-builders/idle-infra/infra-console/internal/auth"
)

// chain builds main.go's wiring: auth middleware outside, the same middleware
// again inside via the protected API handler.
func chain(t *testing.T, backend http.Handler, issuer string) http.Handler {
	t.Helper()
	verifier := auth.NewVerifier(issuer, nil)
	if !verifier.Configured() {
		t.Fatal("verifier must be configured for this test to mean anything")
	}
	protected := auth.Middleware(verifier, nil)(backend)
	return auth.Middleware(verifier, nil)(Handler(protected, "/v1", "test"))
}

// header injects a credential the way a real MCP client would.
type header struct {
	base  http.RoundTripper
	key   string
	value string
}

func (h header) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set(h.key, h.value)
	return h.base.RoundTrip(clone)
}

func connect(t *testing.T, srv *httptest.Server, token string) (*mcpsdk.ClientSession, error) {
	t.Helper()
	hc := &http.Client{Transport: header{base: http.DefaultTransport, key: "Authorization", value: "Bearer " + token}}
	transport := &mcpsdk.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "auth-integration", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), transport, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func TestMCPRejectsAnUnauthenticatedClient(t *testing.T) {
	issuer := os.Getenv("INTEG_ZITADEL_URL")
	if issuer == "" {
		t.Skip("INTEG_ZITADEL_URL not set; run integration/up.sh first")
	}
	backend, _ := realBackend(t)

	srv := httptest.NewServer(chain(t, backend, issuer))
	t.Cleanup(srv.Close)

	if _, err := connect(t, srv, ""); err == nil {
		t.Fatal("an unauthenticated MCP client must not establish a session")
	}
}

// A real operator credential must reach real data through the deployed chain.
//
// This is the assertion the suite lacked. It previously proved only that a
// scopeless token was refused, and faked the grant half by injecting a
// synthetic principal — so a model in which NO credential could ever be
// admitted still looked green.
func TestMCPAdmitsARealOperator(t *testing.T) {
	issuer := os.Getenv("INTEG_ZITADEL_URL")
	token := os.Getenv("INTEG_ZITADEL_TOKEN")
	if issuer == "" || token == "" {
		t.Skip("INTEG_ZITADEL_URL/INTEG_ZITADEL_TOKEN not set; run integration/up.sh first")
	}
	backend, ns := realBackend(t)

	srv := httptest.NewServer(chain(t, backend, issuer))
	t.Cleanup(srv.Close)

	cs, err := connect(t, srv, token)
	if err != nil {
		t.Fatalf("an operator must establish a session: %v", err)
	}
	text := callProject(t, cs, ns)

	if strings.Contains(text, "HTTP 4") {
		t.Fatalf("an operator was refused: %s", text)
	}
	if !strings.Contains(text, ns) {
		t.Errorf("expected the real project in the response: %s", text)
	}
}

// A real credential without the operator membership is refused.
func TestMCPRefusesARealNonOperator(t *testing.T) {
	issuer := os.Getenv("INTEG_ZITADEL_URL")
	plain := os.Getenv("INTEG_ZITADEL_PLAIN_TOKEN")
	if issuer == "" || plain == "" {
		t.Skip("INTEG_ZITADEL_PLAIN_TOKEN not set; run integration/up.sh first")
	}
	backend, ns := realBackend(t)

	srv := httptest.NewServer(chain(t, backend, issuer))
	t.Cleanup(srv.Close)

	cs, err := connect(t, srv, plain)
	if err != nil {
		return // refused at session establishment is an acceptable refusal
	}
	text := callProject(t, cs, ns)
	if !strings.Contains(text, "HTTP 403") {
		t.Fatalf("a non-operator must be refused, got: %s", text)
	}
}

// A tool call is authorized as the caller who sent it, not as whoever opened
// the session.
//
// The SDK hands every tool handler the INITIALIZE request's context for the
// life of the session. Authorizing from it meant anyone holding a valid token
// plus the session id — a plain header — inherited the initiator's authority,
// and every audited row read named the wrong person.
func TestMCPAuthorizesThePerCallCredentialNotTheSessions(t *testing.T) {
	issuer := os.Getenv("INTEG_ZITADEL_URL")
	operator := os.Getenv("INTEG_ZITADEL_TOKEN")
	plain := os.Getenv("INTEG_ZITADEL_PLAIN_TOKEN")
	if issuer == "" || operator == "" || plain == "" {
		t.Skip("integration environment not set; run integration/up.sh first")
	}
	backend, ns := realBackend(t)

	srv := httptest.NewServer(chain(t, backend, issuer))
	t.Cleanup(srv.Close)

	// Session opened by the OPERATOR; the tool call carries the NON-operator.
	sessionID := initializeSession(t, srv, operator)
	body := rawToolCall(t, srv, plain, sessionID, ns)

	// Refusal may come from either gate: the outer middleware rejects the POST
	// on its own credential, or the inner one rejects the synthesized request.
	// The property under test is that the call was NOT evaluated as the
	// operator who opened the session, so what matters is that no project data
	// came back.
	refused := strings.Contains(body, "403") || strings.Contains(body, "401")
	if !refused || strings.Contains(body, `"namespace"`) {
		t.Fatalf("the tool call inherited the session initiator's authority instead of "+
			"being evaluated as its own caller: %s", body)
	}
}

func callProject(t *testing.T, cs *mcpsdk.ClientSession, ns string) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "infra_project_overview",
		Arguments: map[string]any{"project": ns},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return toolText(res)
}

// initializeSession drives the streamable HTTP transport by hand, because the
// point is to send the tool call with a DIFFERENT credential than the one that
// opened the session — which a well-behaved client will not do.
func initializeSession(t *testing.T, srv *httptest.Server, token string) string {
	t.Helper()
	const init = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":` +
		`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`

	resp := post(t, srv, token, "", init)
	defer func() { _ = resp.Body.Close() }()
	id := resp.Header.Get("Mcp-Session-Id")
	if id == "" {
		t.Fatal("no Mcp-Session-Id returned")
	}

	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	r2 := post(t, srv, token, id, notif)
	_ = r2.Body.Close()
	return id
}

func rawToolCall(t *testing.T, srv *httptest.Server, token, sessionID, ns string) string {
	t.Helper()
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"infra_project_overview",` +
		`"arguments":{"project":"` + ns + `"}}}`
	resp := post(t, srv, token, sessionID, call)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

func post(t *testing.T, srv *httptest.Server, token, sessionID, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

func toolText(res *mcpsdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
