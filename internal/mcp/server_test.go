package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type envelope struct {
	Answer     json.RawMessage `json:"answer"`
	Summary    string          `json:"summary"`
	Confidence float64         `json:"confidence"`
	Truncated  bool            `json:"truncated"`
	Provenance []struct {
		File string `json:"file"`
		Line int    `json:"line"`
		Rule string `json:"rule"`
	} `json:"provenance"`
}

type toolResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestJSONRPCRoundTripOverPipes drives the real server through stdin/stdout
// pipes: initialize, tools/list, and one call per tool plus the error paths.
func TestJSONRPCRoundTripOverPipes(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.ts", "export function add(a: number, b: number): number {\n  return a + b;\n}\n")
	writeTestFile(t, dir, "b.ts", "import { add } from \"./a\";\n\nexport function sum(): number {\n  return add(1, 2);\n}\n")
	writeTestFile(t, dir, "c.ts", "import { sum } from \"./b\";\n\nexport function app(): number {\n  return sum();\n}\n")

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	done := make(chan int, 1)
	go func() {
		code := Run(context.Background(), dir, "test-version", stdinR, stdoutW, io.Discard)
		_ = stdoutW.Close()
		done <- code
	}()

	responses := make(chan rpcResponse, 64)
	go func() {
		defer close(responses)
		scanner := bufio.NewScanner(stdoutR)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var resp rpcResponse
			if err := json.Unmarshal(line, &resp); err != nil {
				return
			}
			responses <- resp
		}
	}()

	requests := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"pipe-test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"find_symbol","arguments":{"name":"add"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"impact","arguments":{"symbol":"add","depth":2}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"path","arguments":{"from":"app","to":"add"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"callers","arguments":{"symbol":"add"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"repo_map","arguments":{"max_tokens":400}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"status","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"find_symbol","arguments":{"name":"nope"}}}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"find_symbol","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"not_a_tool","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":12,"method":"no/such/method"}`,
	}
	for _, request := range requests {
		if _, err := fmt.Fprintln(stdinW, request); err != nil {
			t.Fatalf("write request: %v", err)
		}
	}
	if err := stdinW.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}

	byID := make(map[string]rpcResponse)
	for resp := range responses {
		byID[string(resp.ID)] = resp
	}
	if code := <-done; code != 0 {
		t.Fatalf("server exit = %d, want 0", code)
	}

	for _, id := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("no response for id %s", id)
		}
	}
	if len(byID) != 12 {
		t.Fatalf("responses = %d, want 12 (notifications must not answer)", len(byID))
	}

	// initialize
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Capabilities struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(byID["1"].Result, &init); err != nil {
		t.Fatalf("initialize result: %v", err)
	}
	if init.ProtocolVersion == "" || init.ServerInfo.Name != "repath" || init.ServerInfo.Version != "test-version" {
		t.Fatalf("initialize = %+v", init)
	}
	if len(init.Capabilities.Tools) == 0 {
		t.Fatal("initialize did not advertise the tools capability")
	}

	// tools/list: exactly the six contract tools, in order.
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(byID["2"].Result, &list); err != nil {
		t.Fatalf("tools/list result: %v", err)
	}
	wantTools := []string{"find_symbol", "impact", "path", "callers", "repo_map", "status"}
	if len(list.Tools) != len(wantTools) {
		t.Fatalf("tools = %d, want %d", len(list.Tools), len(wantTools))
	}
	for i, want := range wantTools {
		if list.Tools[i].Name != want {
			t.Fatalf("tool %d = %q, want %q", i, list.Tools[i].Name, want)
		}
		if list.Tools[i].InputSchema["type"] != "object" {
			t.Fatalf("tool %s has no object input schema", want)
		}
	}

	find := decodeTool(t, byID["3"])
	if find.IsError {
		t.Fatalf("find_symbol returned an error: %s", find.Env.Summary)
	}
	var findAnswer struct {
		Symbol struct {
			QName string `json:"qname"`
			File  string `json:"file"`
		} `json:"symbol"`
		Callers int `json:"callers"`
		Callees int `json:"callees"`
	}
	if err := json.Unmarshal(find.Env.Answer, &findAnswer); err != nil {
		t.Fatalf("find answer: %v", err)
	}
	if findAnswer.Symbol.QName != "add" || findAnswer.Callers != 1 || findAnswer.Callees != 0 {
		t.Fatalf("find answer = %+v", findAnswer)
	}
	if find.Env.Summary == "" || find.Env.Confidence != 1 || len(find.Env.Provenance) == 0 {
		t.Fatalf("find envelope = %+v", find.Env)
	}

	impact := decodeTool(t, byID["4"])
	var impactAnswer struct {
		Dependents []struct {
			QName string  `json:"qname"`
			Depth int     `json:"depth"`
			Rule  string  `json:"rule"`
			Conf  float64 `json:"confidence"`
		} `json:"dependents"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(impact.Env.Answer, &impactAnswer); err != nil {
		t.Fatalf("impact answer: %v", err)
	}
	if len(impactAnswer.Dependents) != 2 {
		t.Fatalf("dependents = %d, want 2: %s", len(impactAnswer.Dependents), impact.Env.Answer)
	}
	depths := map[string]int{}
	for _, d := range impactAnswer.Dependents {
		depths[d.QName] = d.Depth
		if d.Rule == "" || d.Conf < 0.99 {
			t.Fatalf("dependent missing provenance: %+v", d)
		}
	}
	if depths["sum"] != 1 || depths["app"] != 2 {
		t.Fatalf("depths = %+v, want sum:1 app:2", depths)
	}
	if len(impact.Env.Provenance) != 2 {
		t.Fatalf("impact provenance = %d, want 2", len(impact.Env.Provenance))
	}

	path := decodeTool(t, byID["5"])
	var pathAnswer struct {
		Hops []struct {
			Rule       string  `json:"rule"`
			Confidence float64 `json:"confidence"`
		} `json:"hops"`
	}
	if err := json.Unmarshal(path.Env.Answer, &pathAnswer); err != nil {
		t.Fatalf("path answer: %v", err)
	}
	if len(pathAnswer.Hops) != 2 {
		t.Fatalf("hops = %d, want 2: %s", len(pathAnswer.Hops), path.Env.Answer)
	}
	if path.Env.Confidence < 0.99 || len(path.Env.Provenance) != 2 {
		t.Fatalf("path envelope = %+v", path.Env)
	}

	callers := decodeTool(t, byID["6"])
	var callersAnswer struct {
		Callers []struct {
			QName      string  `json:"qname"`
			Confidence float64 `json:"confidence"`
			Rule       string  `json:"rule"`
		} `json:"callers"`
	}
	if err := json.Unmarshal(callers.Env.Answer, &callersAnswer); err != nil {
		t.Fatalf("callers answer: %v", err)
	}
	if len(callersAnswer.Callers) != 1 || callersAnswer.Callers[0].QName != "sum" {
		t.Fatalf("callers answer = %s", callers.Env.Answer)
	}

	repoMap := decodeTool(t, byID["7"])
	var mapAnswer struct {
		Modules []struct {
			Name string `json:"name"`
		} `json:"modules"`
		Top []struct {
			QName string `json:"qname"`
		} `json:"top"`
	}
	if err := json.Unmarshal(repoMap.Env.Answer, &mapAnswer); err != nil {
		t.Fatalf("repo_map answer: %v", err)
	}
	if len(mapAnswer.Modules) == 0 || len(mapAnswer.Top) == 0 {
		t.Fatalf("repo_map is empty: %s", repoMap.Env.Answer)
	}

	status := decodeTool(t, byID["8"])
	var statusAnswer struct {
		Files         int `json:"files"`
		Nodes         int `json:"nodes"`
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(status.Env.Answer, &statusAnswer); err != nil {
		t.Fatalf("status answer: %v", err)
	}
	if statusAnswer.Files != 3 || statusAnswer.Nodes != 3 || statusAnswer.SchemaVersion != 6 {
		t.Fatalf("status answer = %+v", statusAnswer)
	}

	// Error paths: not found, missing argument, unknown tool.
	for _, id := range []string{"9", "10", "11"} {
		resp := decodeTool(t, byID[id])
		if !resp.IsError {
			t.Fatalf("id %s did not set isError", id)
		}
		var errAnswer struct {
			Code int    `json:"code"`
			Err  string `json:"error"`
		}
		if err := json.Unmarshal(resp.Env.Answer, &errAnswer); err != nil {
			t.Fatalf("error answer id %s: %v", id, err)
		}
		if errAnswer.Err == "" {
			t.Fatalf("id %s has no error message", id)
		}
	}
	var notFound struct {
		Code int `json:"code"`
	}
	_ = json.Unmarshal(decodeTool(t, byID["9"]).Env.Answer, &notFound)
	if notFound.Code != 3 {
		t.Fatalf("not-found code = %d, want 3", notFound.Code)
	}

	// Unknown method is a JSON-RPC protocol error, not a tool error.
	if byID["12"].Error == nil || byID["12"].Error.Code != -32601 {
		t.Fatalf("unknown method response = %+v", byID["12"])
	}
}

type decodedTool struct {
	Env     envelope
	IsError bool
}

func decodeTool(t *testing.T, resp rpcResponse) decodedTool {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %+v", resp.Error)
	}
	var result toolResponse
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("tool result: %v\n%s", err, resp.Result)
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("tool content = %+v", result.Content)
	}
	var env envelope
	if err := json.Unmarshal([]byte(result.Content[0].Text), &env); err != nil {
		t.Fatalf("envelope: %v\n%s", err, result.Content[0].Text)
	}
	if env.Summary == "" {
		t.Fatalf("envelope has no summary: %s", result.Content[0].Text)
	}
	if env.Provenance == nil {
		t.Fatalf("envelope provenance is null: %s", result.Content[0].Text)
	}
	return decodedTool{Env: env, IsError: result.IsError}
}

func TestMainRejectsUnknownFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Main([]string{"--nope"}, "test", bytes.NewReader(nil), &out, &errOut)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", out.String())
	}
}

type modernResponse struct {
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	} `json:"error"`
}

func runServerLines(t *testing.T, dir string, requests ...string) []modernResponse {
	t.Helper()
	var out bytes.Buffer
	s := &server{root: dir, version: "test-version", out: &out, stderr: io.Discard}
	defer s.close()

	for _, request := range requests {
		s.handle(context.Background(), []byte(request))
	}
	var responses []modernResponse
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var resp modernResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("decode response %s: %v", line, err)
		}
		responses = append(responses, resp)
	}
	return responses
}

func TestModernProtocolNegotiation(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "a.ts", "export function add(a: number, b: number): number { return a + b; }\n")

	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","clientInfo":{"name":"modern-test","version":"1"},"clientCapabilities":{}}`
	responses := runServerLines(t, dir,
		`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{`+meta+`}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"impact","arguments":{"symbol":"add"},`+meta+`}}`,
		`{"jsonrpc":"2.0","id":4,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"legacy-test","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":7,"method":"no/such/method","params":{`+meta+`}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"not_a_tool","arguments":{},`+meta+`}}`,
	)
	if len(responses) != 8 {
		t.Fatalf("responses = %d, want 8", len(responses))
	}

	discover := responses[0].Result
	if discover["resultType"] != "complete" {
		t.Fatalf("discover resultType = %v", discover["resultType"])
	}
	versions, _ := discover["supportedVersions"].([]any)
	if len(versions) != 1 || versions[0] != "2026-07-28" {
		t.Fatalf("supportedVersions = %+v", discover["supportedVersions"])
	}
	metaOut, _ := discover["_meta"].(map[string]any)
	if metaOut == nil {
		t.Fatalf("discover _meta missing: %+v", discover)
	}
	serverInfo, _ := metaOut["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if serverInfo["name"] != "repath" || serverInfo["version"] != "test-version" {
		t.Fatalf("serverInfo = %+v", serverInfo)
	}

	list := responses[1].Result
	if list["resultType"] != "complete" {
		t.Fatalf("tools/list resultType = %v", list["resultType"])
	}
	tools, _ := list["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("tools = %d, want 6", len(tools))
	}

	call := responses[2].Result
	if call["resultType"] != "complete" {
		t.Fatalf("tools/call resultType = %v", call["resultType"])
	}
	content, _ := call["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tools/call content = %+v", call)
	}

	if responses[3].Error == nil || responses[3].Error.Code != -32022 {
		t.Fatalf("unsupported version error = %+v", responses[3].Error)
	}
	if got := responses[3].Error.Data["requested"]; got != "1900-01-01" {
		t.Fatalf("unsupported version data = %+v", responses[3].Error.Data)
	}

	if responses[4].Result["protocolVersion"] != "2024-11-05" {
		t.Fatalf("legacy initialize = %+v", responses[4].Result)
	}
	if _, ok := responses[5].Result["resultType"]; ok {
		t.Fatalf("legacy tools/list unexpectedly has resultType: %+v", responses[5].Result)
	}
	if responses[6].Error == nil || responses[6].Error.Code != -32601 {
		t.Fatalf("unknown modern method error = %+v", responses[6].Error)
	}
	toolErr := responses[7].Result
	if toolErr["resultType"] != "complete" || toolErr["isError"] != true {
		t.Fatalf("modern tool error = %+v", toolErr)
	}
}
