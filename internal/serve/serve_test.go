package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/output"
	"github.com/Goutam-11/repath/internal/store"
)

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// buildRoot indexes a small TS graph and closes the writer, so the serve
// package can open it read-only.
func buildRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "a.ts", "export function add(a: number, b: number): number {\n  return a + b;\n}\n")
	writeFile(t, root, "b.ts", "import { add } from \"./a\";\nexport function sum(): number {\n  return add(1, 2);\n}\n")
	writeFile(t, root, "c.ts", "import { sum } from \"./b\";\nexport function app(): number {\n  return sum();\n}\n")
	writeFile(t, root, "run1.ts", "export function run(): void {}\n")
	writeFile(t, root, "run2.ts", "export function run(): void {}\n")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func newTestServer(t *testing.T, root string) *httptest.Server {
	t.Helper()
	srv, err := New(context.Background(), root, "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		srv.Close()
	})
	return ts
}

func getEnvelope(t *testing.T, url string) (int, output.Envelope) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var envelope output.Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("response is not an envelope: %v\n%s", err, data)
	}
	if envelope.Provenance == nil && resp.StatusCode == http.StatusOK {
		t.Fatalf("successful answer has nil provenance: %s", data)
	}
	return resp.StatusCode, envelope
}

func TestSixEndpointsShareTheEnvelope(t *testing.T) {
	ts := newTestServer(t, buildRoot(t))

	cases := []struct {
		path       string
		wantStatus int
		check      func(t *testing.T, answer map[string]any)
	}{
		{"/find?name=add", 200, func(t *testing.T, answer map[string]any) {
			if _, ok := answer["symbol"]; !ok {
				t.Fatalf("find answer = %+v", answer)
			}
		}},
		{"/impact?symbol=add", 200, func(t *testing.T, answer map[string]any) {
			dependents, _ := answer["dependents"].([]any)
			if len(dependents) != 2 {
				t.Fatalf("impact dependents = %+v", answer["dependents"])
			}
		}},
		{"/path?from=app&to=add", 200, func(t *testing.T, answer map[string]any) {
			hops, _ := answer["hops"].([]any)
			if len(hops) != 2 {
				t.Fatalf("path hops = %+v", answer["hops"])
			}
		}},
		{"/callers?symbol=add", 200, func(t *testing.T, answer map[string]any) {
			callers, _ := answer["callers"].([]any)
			if len(callers) != 1 {
				t.Fatalf("callers = %+v", answer["callers"])
			}
		}},
		{"/map?max_tokens=1200", 200, func(t *testing.T, answer map[string]any) {
			if _, ok := answer["modules"]; !ok {
				t.Fatalf("map answer = %+v", answer)
			}
		}},
		{"/status", 200, func(t *testing.T, answer map[string]any) {
			if answer["files"] != float64(5) {
				t.Fatalf("status files = %+v", answer["files"])
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			status, envelope := getEnvelope(t, ts.URL+tc.path)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d", status, tc.wantStatus)
			}
			answer, ok := envelope.Answer.(map[string]any)
			if !ok {
				t.Fatalf("answer is %T", envelope.Answer)
			}
			tc.check(t, answer)
			if envelope.Summary == "" || envelope.Confidence < 0 || envelope.Confidence > 1 {
				t.Fatalf("envelope = %+v", envelope)
			}
		})
	}
}

func TestStatusDoesNotRefresh(t *testing.T) {
	root := buildRoot(t)
	db := filepath.Join(root, ".repath", "index.db")
	before, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestServer(t, root)

	if status, _ := getEnvelope(t, ts.URL+"/status"); status != 200 {
		t.Fatalf("status = %d", status)
	}
	after, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Fatalf("serve wrote the index: %v/%d -> %v/%d",
			before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
}

func TestErrorsMapToHTTPStatus(t *testing.T) {
	ts := newTestServer(t, buildRoot(t))

	status, envelope := getEnvelope(t, ts.URL+"/find?name=missing")
	if status != http.StatusNotFound {
		t.Fatalf("missing status = %d", status)
	}
	answer := envelope.Answer.(map[string]any)
	if answer["code"] != float64(3) {
		t.Fatalf("missing answer = %+v", answer)
	}

	status, envelope = getEnvelope(t, ts.URL+"/find?name=run")
	if status != http.StatusConflict {
		t.Fatalf("ambiguous status = %d, want 409", status)
	}
	answer = envelope.Answer.(map[string]any)
	candidates, _ := answer["candidates"].([]any)
	if answer["code"] != float64(4) || len(candidates) != 2 {
		t.Fatalf("ambiguous answer = %+v", answer)
	}

	for _, path := range []string{"/impact", "/impact?symbol=add&depth=x", "/impact?symbol=add&min_confidence=2", "/callers?symbol=add&depth=2"} {
		if status, _ := getEnvelope(t, ts.URL+path); status != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", path, status)
		}
	}
	if status, _ := getEnvelope(t, ts.URL+"/nope"); status != http.StatusNotFound {
		t.Fatalf("unknown path status = %d", status)
	}

	resp, err := http.Post(ts.URL+"/impact?symbol=add", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}
}

func TestConcurrentReads(t *testing.T) {
	ts := newTestServer(t, buildRoot(t))
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(ts.URL + "/impact?symbol=add")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- errors.New("non-200 status")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestNewRefusesMissingIndex(t *testing.T) {
	_, err := New(context.Background(), t.TempDir(), "test")
	if !errors.Is(err, ErrNoIndex) {
		t.Fatalf("error = %v, want ErrNoIndex", err)
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	root := buildRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, root, "127.0.0.1:0", "test", func(addr string) { ready <- addr }, io.Discard)
	}()

	var addr string
	select {
	case addr = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}
	resp, err := http.Get("http://" + addr + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
	if _, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
		t.Fatal("server still accepts connections after shutdown")
	}
}
