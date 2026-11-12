// Package serve is the read-only HTTP surface from PLAN §16.8. It exposes the
// same six answers as MCP and the CLI, using the shared query/output layers.
// It opens the existing index read-only (SQLite mode=ro + query_only) and never
// indexes, refreshes, migrates, or writes an artifact.
package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Goutam-11/repath/internal/output"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
)

// ErrNoIndex marks a missing index. The CLI maps it to exit code 3.
var ErrNoIndex = errors.New("no index")

// Server owns one read-only store and the six answer handlers.
type Server struct {
	store   *store.Store
	root    string
	version string
}

// New opens <root>/.repath/index.db read-only. It refuses a missing index or a
// schema it does not understand instead of creating or migrating one.
func New(ctx context.Context, root, version string) (*Server, error) {
	dbPath := filepath.Join(root, ".repath", "index.db")
	st, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s (run repath index or status first)", ErrNoIndex, dbPath)
		}
		return nil, err
	}

	meta, err := st.Meta(ctx)
	if err != nil {
		st.Close()
		return nil, err
	}
	if got := meta["schema_version"]; got != strconv.Itoa(store.SchemaVersion) {
		st.Close()
		return nil, fmt.Errorf("index schema v%s is not supported by this binary (expects v%d); rebuild the index",
			got, store.SchemaVersion)
	}
	return &Server{store: st, root: root, version: version}, nil
}

// Close closes the read-only store.
func (s *Server) Close() error { return s.store.Close() }

// Handler returns the read-only GET surface. Exactly six answers are exposed;
// there is no mutation route or artifact route.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeEnvelope(w, http.StatusMethodNotAllowed, query.ErrorEnvelope(2, "method not allowed: "+r.Method, nil))
			return
		}
		switch r.URL.Path {
		case "/find":
			s.find(w, r)
		case "/impact":
			s.impact(w, r)
		case "/path":
			s.path(w, r)
		case "/callers":
			s.callers(w, r)
		case "/map":
			s.mapAnswer(w, r)
		case "/status":
			s.status(w, r)
		default:
			writeEnvelope(w, http.StatusNotFound, query.ErrorEnvelope(3, "not found: "+r.URL.Path, nil))
		}
	})
}

func (s *Server) find(w http.ResponseWriter, r *http.Request) {
	name, err := requiredQuery(r, "name")
	if err != nil {
		writeUsage(w, err)
		return
	}
	answer, code, err := query.Find(r.Context(), s.store, name, r.URL.Query().Get("module"), query.DefaultMinConfidence)
	if err != nil {
		writeInternal(w)
		return
	}
	if code != 0 {
		writeTargetError(w, code, name, answer.Target)
		return
	}
	writeEnvelope(w, http.StatusOK, query.FindEnvelope(answer))
}

func (s *Server) impact(w http.ResponseWriter, r *http.Request) {
	symbol, err := requiredQuery(r, "symbol")
	if err != nil {
		writeUsage(w, err)
		return
	}
	depth, err := intQuery(r, "depth", query.DefaultDepth)
	if err != nil {
		writeUsage(w, err)
		return
	}
	minConfidence, err := floatQuery(r, "min_confidence", query.DefaultMinConfidence)
	if err != nil {
		writeUsage(w, err)
		return
	}
	answer, code, err := query.Impact(r.Context(), s.store, symbol, depth, minConfidence, query.DefaultLimit)
	if err != nil {
		writeInternal(w)
		return
	}
	if code != 0 {
		writeTargetError(w, code, symbol, answer.Target)
		return
	}
	writeEnvelope(w, http.StatusOK, query.ImpactEnvelope(answer))
}

func (s *Server) path(w http.ResponseWriter, r *http.Request) {
	from, err := requiredQuery(r, "from")
	if err != nil {
		writeUsage(w, err)
		return
	}
	to, err := requiredQuery(r, "to")
	if err != nil {
		writeUsage(w, err)
		return
	}
	maxDepth, err := intQuery(r, "max_depth", query.DefaultDepth)
	if err != nil {
		writeUsage(w, err)
		return
	}
	minConfidence, err := floatQuery(r, "min_confidence", query.DefaultMinConfidence)
	if err != nil {
		writeUsage(w, err)
		return
	}
	answer, code, err := query.Path(r.Context(), s.store, from, to, maxDepth, minConfidence, 3)
	if err != nil {
		writeInternal(w)
		return
	}
	if code != 0 {
		if answer.Failed != nil {
			writeTargetError(w, answer.Failed.Code, answer.Failed.Arg, answer.Failed.Target)
			return
		}
		writeEnvelope(w, http.StatusBadRequest, query.ErrorEnvelope(code, "could not resolve path endpoints", nil))
		return
	}
	writeEnvelope(w, http.StatusOK, query.PathEnvelope(answer))
}

func (s *Server) callers(w http.ResponseWriter, r *http.Request) {
	symbol, err := requiredQuery(r, "symbol")
	if err != nil {
		writeUsage(w, err)
		return
	}
	depth, err := intQuery(r, "depth", 1)
	if err != nil {
		writeUsage(w, err)
		return
	}
	if depth != 1 {
		writeUsage(w, fmt.Errorf("callers supports depth 1; use /impact for transitive reach"))
		return
	}
	answer, code, err := query.Callers(r.Context(), s.store, symbol, query.DefaultMinConfidence, query.DefaultCallersLimit)
	if err != nil {
		writeInternal(w)
		return
	}
	if code != 0 {
		writeTargetError(w, code, symbol, answer.Target)
		return
	}
	writeEnvelope(w, http.StatusOK, query.CallersEnvelope(answer))
}

func (s *Server) mapAnswer(w http.ResponseWriter, r *http.Request) {
	maxTokens, err := intQuery(r, "max_tokens", query.DefaultMaxTokens)
	if err != nil {
		writeUsage(w, err)
		return
	}
	answer, err := query.Map(r.Context(), s.store, maxTokens, query.DefaultMinConfidence)
	if err != nil {
		writeInternal(w)
		return
	}
	writeEnvelope(w, http.StatusOK, query.MapEnvelope(answer))
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	// No refresh: serve is read-only. Staleness is reported in the answer.
	report, err := query.Status(r.Context(), s.store, s.root, false, false)
	if err != nil {
		writeInternal(w)
		return
	}
	report.Version = s.version
	writeEnvelope(w, http.StatusOK, query.StatusEnvelope(report))
}

func writeEnvelope(w http.ResponseWriter, status int, envelope output.Envelope) {
	data, err := output.Marshal(envelope)
	if err != nil {
		http.Error(w, "encode answer", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func writeUsage(w http.ResponseWriter, err error) {
	writeEnvelope(w, http.StatusBadRequest, query.ErrorEnvelope(2, err.Error(), nil))
}

func writeInternal(w http.ResponseWriter) {
	writeEnvelope(w, http.StatusInternalServerError, query.ErrorEnvelope(1, "internal error", nil))
}

func writeTargetError(w http.ResponseWriter, code int, name string, target query.Target) {
	switch code {
	case 3:
		writeEnvelope(w, http.StatusNotFound, query.ErrorEnvelope(3, "not found: "+name, nil))
	case 4:
		candidates := make([]query.ErrorCandidate, 0, len(target.Candidates))
		for _, node := range target.Candidates {
			candidates = append(candidates, query.ErrorCandidate{QName: node.QName, Kind: node.Kind})
		}
		writeEnvelope(w, http.StatusConflict, query.ErrorEnvelope(4,
			fmt.Sprintf("ambiguous: %s matches %d symbols", name, len(target.Candidates)), candidates))
	default:
		writeEnvelope(w, http.StatusBadRequest, query.ErrorEnvelope(code, "could not resolve target", nil))
	}
}

func requiredQuery(r *http.Request, key string) (string, error) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return "", fmt.Errorf("missing required query parameter: %s", key)
	}
	return value, nil
}

func intQuery(r *http.Request, key string, def int) (int, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return def, fmt.Errorf("query parameter %s must be a non-negative integer", key)
	}
	return value, nil
}

func floatQuery(r *http.Request, key string, def float64) (float64, error) {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 || value > 1 {
		return def, fmt.Errorf("query parameter %s must be between 0 and 1", key)
	}
	return value, nil
}

// Run opens the read-only index, listens on addr, and serves until ctx is
// cancelled (SIGINT/SIGTERM in the CLI). ready is called with the bound address
// after the listener is up. A non-loopback bind is warned about on stderr.
func Run(ctx context.Context, root, addr, version string,
	ready func(addr string), stderr io.Writer) error {

	srv, err := New(ctx, root, version)
	if err != nil {
		return err
	}
	defer srv.Close()

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	if !isLoopback(listener.Addr()) {
		fmt.Fprintf(stderr, "warning: repath serve is bound to %s, not loopback; it has no authentication\n", listener.Addr())
	}

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	if ready != nil {
		ready(listener.Addr().String())
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
		}
		err := <-serveErr
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func isLoopback(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
