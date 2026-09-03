package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/upstacked/cli/internal/errs"
)

// stubServer is a fake Upstacked API. Routes are keyed by method and path so
// a test can give the same path different behaviour per verb, which is how the
// real API works.
type stubServer struct {
	*httptest.Server
	routes   map[string]http.HandlerFunc
	requests []recordedRequest
}

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
	Auth   string
}

func newStub(t *testing.T) *stubServer {
	t.Helper()
	s := &stubServer{routes: map[string]http.HandlerFunc{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{
			Method: r.Method, Path: r.URL.Path,
			Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization"),
		}
		if r.Body != nil {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.Body = body
		}
		s.requests = append(s.requests, rec)

		if h, ok := s.routes[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		if h, ok := s.routes["* "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		// An unregistered route is a test bug, and saying so beats a 404 that
		// the CLI reports as a missing resource.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"detail": "no stub registered for " + r.Method + " " + r.URL.Path,
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// handle registers a JSON response for any method on a path.
func (s *stubServer) handle(path string, status int, body any) {
	s.routes["* "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

// handleMethod registers a JSON response for one method on a path.
func (s *stubServer) handleMethod(method, path string, status int, body any) {
	s.routes[method+" "+path] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}
}

// handleFunc registers a custom handler for any method on a path.
func (s *stubServer) handleFunc(path string, fn http.HandlerFunc) {
	s.routes["* "+path] = fn
}

// page wraps items in the paginated envelope the API uses.
func page(items ...any) map[string]any {
	if items == nil {
		items = []any{}
	}
	return map[string]any{"count": len(items), "next": nil, "previous": nil, "results": items}
}

func (s *stubServer) requestsTo(method, path string) []recordedRequest {
	var out []recordedRequest
	for _, r := range s.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// result is the observable outcome of a command: what a user or script sees.
type result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

func (r result) JSON(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &m); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.Stdout)
	}
	return m
}

// env is one isolated CLI installation.
type env struct {
	t         *testing.T
	dir       string
	stub      *stubServer
	skillHome string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	home := t.TempDir()
	// Skill installation and credential storage must not touch the real home.
	t.Setenv("HOME", home)
	t.Setenv("UPS_CONFIG_DIR", dir)
	t.Setenv("NO_COLOR", "1")
	t.Setenv("UPSTACKED_TOKEN", "")
	t.Setenv("UPSTACKED_API_URL", "")
	t.Setenv("UPSTACKED_INFRASTRUCTURE", "")
	return &env{t: t, dir: dir, stub: newStub(t), skillHome: home}
}

// run executes a command exactly as a user would, and captures everything.
func (e *env) run(args ...string) result {
	e.t.Helper()
	var out, errOut bytes.Buffer

	// A plain empty file, not /dev/null: a character device would read as
	// interactive under a naive TTY check and mask prompting bugs.
	empty, err := os.CreateTemp(e.t.TempDir(), "stdin")
	if err != nil {
		e.t.Fatal(err)
	}
	defer empty.Close()

	app := &App{Stdin: empty, Stdout: &out, Stderr: &errOut, ConfigDir: e.dir}
	root := NewRoot(app)
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)

	code := errs.CodeOK
	if err := root.Execute(); err != nil {
		code = reportErrorTo(app, err, &errOut)
	}
	return result{Stdout: out.String(), Stderr: errOut.String(), ExitCode: code}
}

// runStdin executes a command with the given text available on stdin.
func (e *env) runStdin(stdin string, args ...string) result {
	e.t.Helper()
	f, err := os.CreateTemp(e.t.TempDir(), "stdin")
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(stdin); err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		e.t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	app := &App{Stdin: f, Stdout: &out, Stderr: &errOut, ConfigDir: e.dir}
	root := NewRoot(app)
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&errOut)

	code := errs.CodeOK
	if err := root.Execute(); err != nil {
		code = reportErrorTo(app, err, &errOut)
	}
	return result{Stdout: out.String(), Stderr: errOut.String(), ExitCode: code}
}

// reportErrorTo mirrors reportError but writes to the captured stream.
func reportErrorTo(app *App, err error, w *bytes.Buffer) int {
	w.WriteString("error: " + err.Error() + "\n")
	if hint := errs.HintOf(err); hint != "" {
		w.WriteString("  hint: " + hint + "\n")
	}
	return errs.CodeOf(err)
}

// login writes a credential directly, so tests that are not about
// authentication do not have to perform it.
func (e *env) login() {
	e.t.Helper()
	res := e.run("profile", "add", "default", "--url", e.stub.URL)
	if res.ExitCode != 0 && !strings.Contains(res.Stderr, "already exists") {
		e.t.Fatalf("profile add failed: %s", res.Stderr)
	}
	e.stub.handle("/api/token/", 200, map[string]any{
		"access": "test-access-token", "refresh": "test-refresh-token", "username": "tester",
	})
	res = e.runStdin("hunter2\n", "login", "--username", "tester", "--password-stdin")
	if res.ExitCode != 0 {
		e.t.Fatalf("login failed: %s", res.Stderr)
	}
}

// setInfra pins the active infrastructure without a network round trip.
func (e *env) setInfra(id string) {
	e.t.Helper()
	res := e.run("context", "set", "--infra-id", id)
	if res.ExitCode != 0 {
		e.t.Fatalf("context set failed: %s", res.Stderr)
	}
}

func contains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected to find %q in:\n%s", needle, haystack)
	}
}

func notContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("did not expect %q in:\n%s", needle, haystack)
	}
}

func jsonEncode(w interface{ Write([]byte) (int, error) }, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// org registers the caller as belonging to exactly one organization.
//
// Every monitoring item and template create needs one: the API's permission
// check reads `organization` off the request body and rejects the request when
// it is absent. Tests that create either must set this up, or they assert
// against a request the real server would refuse.
func (e *env) org(id string) {
	e.t.Helper()
	e.stub.handleMethod("GET", "/api/user/details/v2/", 200, map[string]any{
		"user":          map[string]any{"username": "tester"},
		"organizations": map[string]any{id: map[string]any{"id": atoiOr(id), "name": "Acme"}},
	})
}
