// Package mcp exposes the console's read surface as MCP tools.
//
// Tools are generated from the contract's x-mcp extension and dispatched
// through the same http.Handler the browser uses, so the two adapters cannot
// drift.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxToolResponse is the backstop that keeps a pathological response from
// filling a model's context; log tails and rows are already bounded.
const maxToolResponse = 256 << 10

// NewServer builds an MCP server whose tools proxy into handler. baseURL is
// the API's mount prefix, so /platform/nodes dispatches to /v1/platform/nodes.
func NewServer(handler http.Handler, baseURL string, version string) *mcpsdk.Server {
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    "infra-console",
		Version: version,
	}, nil)

	for _, tool := range Tools {
		addTool(srv, handler, strings.TrimRight(baseURL, "/"), tool)
	}
	return srv
}

func addTool(srv *mcpsdk.Server, handler http.Handler, baseURL string, tool Tool) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(tool.InputSchema), &schema); err != nil {
		// A malformed generated schema is a build-time bug; advertise the tool
		// with an open schema rather than dropping it silently.
		schema = map[string]any{"type": "object"}
	}

	definition := &mcpsdk.Tool{
		Name:        tool.Name,
		Description: tool.Description,
		InputSchema: schema,
		Annotations: &mcpsdk.ToolAnnotations{
			// Every operation in this contract is a GET.
			ReadOnlyHint: true,
		},
	}

	mcpsdk.AddTool(srv, definition, func(ctx context.Context, req *mcpsdk.CallToolRequest, args map[string]any) (*mcpsdk.CallToolResult, any, error) {
		target, err := buildURL(baseURL, tool, args)
		if err != nil {
			return errorResult(err.Error()), nil, nil
		}

		httpReq := httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)

		// Carry THIS call's credential, not the session's. The SDK hands every
		// tool handler the INITIALIZE request's context, so authorizing from
		// it let anyone with a valid token plus the session id inherit the
		// initiator's authority — and misattributed every audited row read.
		if req != nil && req.Extra != nil && req.Extra.Header != nil {
			if authz := req.Extra.Header.Get("Authorization"); authz != "" {
				httpReq.Header.Set("Authorization", authz)
			}
			if cookie := req.Extra.Header.Get("Cookie"); cookie != "" {
				httpReq.Header.Set("Cookie", cookie)
			}
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httpReq)

		body, err := io.ReadAll(io.LimitReader(rec.Body, maxToolResponse))
		if err != nil {
			return errorResult("read response: " + err.Error()), nil, nil
		}

		// A non-2xx is data the model should see, not a transport failure:
		// "this source is unavailable" is a useful answer.
		text := string(body)
		if rec.Code >= 400 {
			text = fmt.Sprintf("HTTP %d\n%s", rec.Code, text)
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
			IsError: rec.Code >= 500,
		}, nil, nil
	})
}

// buildURL substitutes path parameters and appends declared query parameters.
// Undeclared ones are dropped, so a tool cannot reach a parameter the contract
// does not define.
func buildURL(baseURL string, tool Tool, args map[string]any) (string, error) {
	path := tool.Path
	for _, name := range tool.PathParams {
		raw, ok := args[name]
		if !ok {
			return "", fmt.Errorf("missing required argument %q", name)
		}
		value := fmt.Sprint(raw)
		if value == "" {
			return "", fmt.Errorf("argument %q must not be empty", name)
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(value))
	}

	query := url.Values{}
	for _, name := range tool.QueryParams {
		raw, ok := args[name]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case []any:
			// Arrays are serialized form-style without explode, matching the
			// contract's `style: form, explode: false`.
			parts := make([]string, 0, len(v))
			for _, item := range v {
				parts = append(parts, fmt.Sprint(item))
			}
			if len(parts) > 0 {
				query.Set(name, strings.Join(parts, ","))
			}
		case bool:
			query.Set(name, fmt.Sprintf("%t", v))
		case float64:
			// JSON numbers decode as float64; emit integers without a decimal.
			if v == float64(int64(v)) {
				query.Set(name, fmt.Sprintf("%d", int64(v)))
			} else {
				query.Set(name, fmt.Sprint(v))
			}
		default:
			query.Set(name, fmt.Sprint(v))
		}
	}

	target := baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return target, nil
}

func errorResult(message string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: message}},
		IsError: true,
	}
}

// Handler returns an HTTP handler serving MCP over the streamable transport,
// suitable for mounting at /mcp.
func Handler(handler http.Handler, baseURL, version string) http.Handler {
	srv := NewServer(handler, baseURL, version)
	// Cross-origin protection on: this endpoint also accepts the session
	// cookie. SameSite=Lax blocks most of it; this closes the rest.
	return mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return srv
	}, &mcpsdk.StreamableHTTPOptions{CrossOriginProtection: http.NewCrossOriginProtection()})
}
