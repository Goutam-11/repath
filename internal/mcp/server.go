// Package mcp is repath's stdio JSON-RPC adapter. It is hand-rolled on purpose
// (PLAN.md §7): the transport is a newline-delimited JSON request/response over
// stdin/stdout, with exactly six answer-shaped tools. The tools call the shared
// query layer, never their own copy of it.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Goutam-11/repath/internal/query"
)

// protocolVersion is the legacy MCP revision this adapter speaks. The six tools
// and the content/isError result shape are stable across the 2024-11 and
// 2025-03 revisions.
const protocolVersion = "2024-11-05"

// modernProtocolVersion is the handshake-free revision. A modern request carries
// its version in params._meta; `server/discover` advertises it for stdio era
// probing. Both eras share the six tools and the envelope.
const modernProtocolVersion = "2026-07-28"

const protocolVersionMetaKey = "io.modelcontextprotocol/protocolVersion"

const usage = `repath mcp — stdio JSON-RPC server for MCP clients

Usage:
  repath mcp [--root DIR]

The server speaks newline-delimited JSON-RPC on stdin/stdout and exposes six
tools: find_symbol, impact, path, callers, repo_map, status.
`

// Main parses the mcp command's flags and runs the server.
func Main(args []string, version string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--root":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "--root needs a directory\n\n%s", usage)
				return 2
			}
			i++
			root = args[i]
		case strings.HasPrefix(args[i], "--root="):
			root = strings.TrimPrefix(args[i], "--root=")
		case args[i] == "--help" || args[i] == "-h":
			fmt.Fprint(stdout, usage)
			return 0
		default:
			fmt.Fprintf(stderr, "unknown argument %q\n\n%s", args[i], usage)
			return 2
		}
	}
	return Run(context.Background(), root, version, stdin, stdout, stderr)
}

// Run serves JSON-RPC until stdin closes. No logging goes to stdout: it is the
// protocol channel.
func Run(ctx context.Context, root, version string, stdin io.Reader, stdout, stderr io.Writer) int {
	s := &server{root: root, version: version, out: stdout, stderr: stderr}
	defer s.close()

	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		s.handle(ctx, line)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(stderr, "mcp: read: %v\n", err)
		return 1
	}
	return 0
}

type server struct {
	root    string
	version string
	out     io.Writer
	stderr  io.Writer
	session *query.Session
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (s *server) handle(ctx context.Context, line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.respondError(nil, -32700, "parse error")
		return
	}

	notification := len(req.ID) == 0
	modern := metaVersion(req.Params) != ""
	if modern && metaVersion(req.Params) != modernProtocolVersion {
		if !notification {
			s.respondErrorData(req.ID, -32022, "Unsupported protocol version", map[string]any{
				"supported": []string{modernProtocolVersion},
				"requested": metaVersion(req.Params),
			})
		}
		return
	}

	switch req.Method {
	case "server/discover":
		if notification {
			return
		}
		s.respond(req.ID, s.discoverResult())
		return

	case "initialize":
		if notification {
			return
		}
		s.respond(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]string{"name": "repath", "version": s.version},
			"instructions": "repath answers structural questions. If you have a shell, " +
				"prefer the CLI (`repath impact|path|map|find|callers`); these six " +
				"tools are for clients without one.",
		})

	case "notifications/initialized", "initialized":
		// Notification: no response.

	case "ping":
		if !notification {
			result := map[string]any{}
			if modern {
				result["resultType"] = "complete"
			}
			s.respond(req.ID, result)
		}

	case "tools/list":
		if notification {
			return
		}
		result := map[string]any{"tools": toolDefinitions()}
		if modern {
			result["resultType"] = "complete"
			result["ttlMs"] = 3600000
			result["cacheScope"] = "public"
		}
		s.respond(req.ID, result)

	case "tools/call":
		if notification {
			return
		}
		s.handleToolCall(ctx, req.ID, req.Params, modern)

	case "shutdown":
		if !notification {
			result := map[string]any{}
			if modern {
				result["resultType"] = "complete"
			}
			s.respond(req.ID, result)
		}

	default:
		if !notification {
			s.respondError(req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func (s *server) handleToolCall(ctx context.Context, id json.RawMessage, raw json.RawMessage, modern bool) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		s.respondError(id, -32602, "invalid params: "+err.Error())
		return
	}
	if params.Name == "" {
		s.respondError(id, -32602, "invalid params: tool name is required")
		return
	}

	result, err := s.callTool(ctx, params.Name, params.Arguments)
	if err != nil {
		res := errorResult(1, err.Error(), nil)
		if _, ok := err.(argumentError); ok {
			res = errorResult(2, err.Error(), nil)
		}
		if modern {
			res.ResultType = "complete"
		}
		s.respond(id, res)
		return
	}
	if modern {
		result.ResultType = "complete"
	}
	s.respond(id, result)
}

// discoverResult is the 2026-07-28 server advertisement. A dual-era server
// lists only the modern revision; legacy clients still reach the initialize
// path out of band.
func (s *server) discoverResult() map[string]any {
	return map[string]any{
		"resultType":        "complete",
		"supportedVersions": []string{modernProtocolVersion},
		"capabilities":      map[string]any{"tools": map[string]any{}},
		"_meta": map[string]any{
			"io.modelcontextprotocol/serverInfo": map[string]string{
				"name": "repath", "version": s.version,
			},
		},
		"instructions": "repath answers structural questions. If you have a shell, " +
			"prefer the CLI (`repath impact|path|map|find|callers`); these six " +
			"tools are for clients without one.",
		"ttlMs":      3600000,
		"cacheScope": "public",
	}
}

// metaVersion reads the modern per-request protocol version. An absent field
// means a legacy client; an unsupported value gets UnsupportedProtocolVersionError.
func metaVersion(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return ""
	}
	value, ok := params.Meta[protocolVersionMetaKey]
	if !ok {
		return ""
	}
	var version string
	if err := json.Unmarshal(value, &version); err != nil {
		return ""
	}
	return version
}

func (s *server) callTool(ctx context.Context, name string, raw json.RawMessage) (toolResult, error) {
	args, err := parseArguments(raw)
	if err != nil {
		return toolResult{}, argumentError("arguments must be an object: " + err.Error())
	}

	switch name {
	case "find_symbol":
		return s.findSymbol(ctx, args)
	case "impact":
		return s.impact(ctx, args)
	case "path":
		return s.path(ctx, args)
	case "callers":
		return s.callers(ctx, args)
	case "repo_map":
		return s.repoMap(ctx, args)
	case "status":
		return s.status(ctx)
	default:
		return toolResult{}, argumentError("unknown tool: " + name)
	}
}

func (s *server) sessionFor(ctx context.Context) (*query.Session, error) {
	if s.session != nil {
		return s.session, nil
	}
	root, err := query.ResolveRoot(s.root)
	if err != nil {
		return nil, fmt.Errorf("resolve root: %w", err)
	}
	session, err := query.Open(ctx, root, false)
	if err != nil {
		return nil, err
	}
	s.session = session
	return session, nil
}

func (s *server) close() {
	if s.session != nil {
		_ = s.session.Close()
		s.session = nil
	}
}

func (s *server) respond(id json.RawMessage, result any) {
	s.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *server) respondError(id json.RawMessage, code int, message string) {
	s.respondErrorData(id, code, message, nil)
}

func (s *server) respondErrorData(id json.RawMessage, code int, message string, data any) {
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}

func (s *server) write(resp response) {
	data, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintf(s.stderr, "mcp: encode: %v\n", err)
		return
	}
	if _, err := s.out.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(s.stderr, "mcp: write: %v\n", err)
	}
}
