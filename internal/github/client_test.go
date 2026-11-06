package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

type call struct {
	Method, Path, Auth string
	Body               map[string]any
}

// fakeGitHub answers by "METHOD path" (path with query). Every request is
// recorded.
type fakeGitHub struct {
	t      *testing.T
	mu     sync.Mutex
	calls  []call
	routes map[string]func(w http.ResponseWriter, c call)
	grant  map[string]string // permissions the fake App holds
}

func newFake(t *testing.T) (*fakeGitHub, *App) {
	f := &fakeGitHub{t: t, routes: map[string]func(http.ResponseWriter, call){}, grant: map[string]string{
		"contents": "write", "issues": "write", "pull_requests": "write", "statuses": "write",
		"actions": "write", "checks": "read", "metadata": "read",
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	f.routes["POST /app/installations/7/access_tokens"] = func(w http.ResponseWriter, c call) {
		perms := map[string]string{}
		for k, v := range c.Body["permissions"].(map[string]any) {
			if g, ok := f.grant[k]; ok {
				if g == "read" && v == "write" {
					v = "read"
				}
				perms[k] = v.(string)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_" + strings.Repeat("x", 8), "expires_at": time.Now().Add(time.Hour), "permissions": perms,
		})
	}
	return f, &App{ID: 42, Key: testKey, BaseURL: srv.URL, HTTP: srv.Client()}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c := call{Method: r.Method, Path: r.URL.RequestURI(), Auth: r.Header.Get("Authorization")}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &c.Body); err != nil {
			f.t.Errorf("%s %s: body is not JSON: %s", r.Method, r.URL, b)
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	h, ok := f.routes[r.Method+" "+c.Path]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
		return
	}
	h(w, c)
}

func (f *fakeGitHub) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeGitHub) ok(route string, body string) {
	f.routes[route] = func(w http.ResponseWriter, _ call) { _, _ = io.WriteString(w, body) }
}

func TestJWTIsSignedByTheAppKey(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := &App{ID: 42, Key: testKey, Now: func() time.Time { return now }}
	tok, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	_ = json.Unmarshal(raw, &claims)
	if claims.Iss != "42" || claims.Iat != now.Unix()-60 || claims.Exp != now.Unix()+540 {
		t.Errorf("claims = %+v", claims)
	}
	if _, err := (&App{ID: 1}).JWT(); err == nil {
		t.Error("JWT without a key")
	}
}

func TestParsePrivateKey(t *testing.T) {
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)})
	der, _ := x509.MarshalPKCS8PrivateKey(testKey)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, b := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		k, err := ParsePrivateKey(b)
		if err != nil || !k.Equal(testKey) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, bad := range []string{"", "not pem", "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----\n"} {
		if _, err := ParsePrivateKey([]byte(bad)); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestWorkerTokenCarriesExactlyWhatWasAsked(t *testing.T) {
	f, app := newFake(t)
	perms := Permissions{"contents": "write", "pull_requests": "write", "issues": "write"}
	tok, err := app.WorkerToken(context.Background(), 7, "demos", perms)
	if err != nil {
		t.Fatal(err)
	}
	c := f.last()
	if !strings.HasPrefix(c.Auth, "Bearer ") {
		t.Errorf("minted with %q, not the App JWT", c.Auth)
	}
	if got := c.Body["repositories"].([]any); len(got) != 1 || got[0] != "demos" {
		t.Errorf("repositories = %v", got)
	}
	got := c.Body["permissions"].(map[string]any)
	if len(got) != len(perms) {
		t.Errorf("permissions sent = %v, want %v", got, perms)
	}
	for k, v := range perms {
		if got[k] != v {
			t.Errorf("permissions[%s] = %v, want %s", k, got[k], v)
		}
	}
	if tok.Value == "" {
		t.Error("no token")
	}

	n := len(f.calls)
	if _, err := app.WorkerToken(context.Background(), 7, "demos", Permissions{"contents": "read", "statuses": "write"}); !errors.Is(err, ErrStatusesForWorker) {
		t.Errorf("statuses: err = %v", err)
	}
	if _, err := app.WorkerToken(context.Background(), 7, "demos", Permissions{"statuses": "read"}); !errors.Is(err, ErrStatusesForWorker) {
		t.Errorf("statuses read: err = %v", err)
	}
	if _, err := app.WorkerToken(context.Background(), 7, "demos", Permissions{}); err == nil {
		t.Error("empty permission set minted")
	}
	if _, err := app.WorkerToken(context.Background(), 7, "o/demos", perms); err == nil {
		t.Error("owner/name repository minted")
	}
	if len(f.calls) != n {
		t.Error("a refused token still reached GitHub")
	}

	// GitHub drops a permission the App lacks; such a token is refused.
	delete(f.grant, "actions")
	if _, err := app.WorkerToken(context.Background(), 7, "demos", Permissions{"actions": "write"}); err == nil {
		t.Error("token missing a permission accepted")
	}
	f.grant["checks"] = "read"
	if _, err := app.WorkerToken(context.Background(), 7, "demos", Permissions{"checks": "write"}); err == nil {
		t.Error("token downgraded to read accepted")
	}
}

func newClient(t *testing.T) (*fakeGitHub, *Client) {
	f, app := newFake(t)
	return f, &Client{App: app, InstallationID: 7, Owner: "showmethebots", Repo: "demos"}
}

func TestClientTokenIsScopedAndReused(t *testing.T) {
	f, c := newClient(t)
	f.ok("POST /repos/showmethebots/demos/issues/12/labels", `[]`)
	for range 3 {
		if err := c.AddLabel(context.Background(), 12, "loop:intake"); err != nil {
			t.Fatal(err)
		}
	}
	mints := 0
	for _, cl := range f.calls {
		if strings.HasSuffix(cl.Path, "/access_tokens") {
			mints++
			if cl.Body["permissions"].(map[string]any)["statuses"] != "write" {
				t.Error("chuck's token lacks statuses")
			}
		} else if cl.Auth != "token ghs_xxxxxxxx" {
			t.Errorf("call made with %q", cl.Auth)
		}
	}
	if mints != 1 {
		t.Errorf("minted %d tokens for three calls", mints)
	}
}

func TestRESTCalls(t *testing.T) {
	ctx := context.Background()
	f, c := newClient(t)
	f.ok("POST /repos/showmethebots/demos/issues/12/labels", `[]`)
	f.ok("DELETE /repos/showmethebots/demos/issues/12/labels/loop:ready", `[]`)
	f.ok("POST /repos/showmethebots/demos/issues/12/comments", `{"html_url":"https://github.com/showmethebots/demos/issues/12#issuecomment-1"}`)
	f.ok("POST /repos/showmethebots/demos/statuses/abc123", `{}`)
	f.ok("POST /repos/showmethebots/demos/actions/workflows/deploy-prod.yml/dispatches", ``)
	f.ok("GET /repos/showmethebots/demos/pulls/13", `{"number":13,"node_id":"PR_1","state":"open","head":{"sha":"abc123","ref":"loop/issue-12"},"user":{"id":99,"login":"chuck[bot]"}}`)
	f.ok("GET /repos/showmethebots/demos/issues/12", `{"number":12,"title":"t","body":"AC-1","state":"open","labels":[{"name":"loop:intake"}]}`)

	check := func(name string, err error, method, path string, body map[string]any) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			return
		}
		got := f.last()
		if got.Method != method || got.Path != path {
			t.Errorf("%s: called %s %s, want %s %s", name, got.Method, got.Path, method, path)
		}
		for k, v := range body {
			if b, _ := json.Marshal(got.Body[k]); string(b) != mustJSON(v) {
				t.Errorf("%s: body[%s] = %s, want %s", name, k, b, mustJSON(v))
			}
		}
	}
	check("add label", c.AddLabel(ctx, 12, "loop:intake"), "POST", "/repos/showmethebots/demos/issues/12/labels", map[string]any{"labels": []string{"loop:intake"}})
	check("remove label", c.RemoveLabel(ctx, 12, "loop:ready"), "DELETE", "/repos/showmethebots/demos/issues/12/labels/loop:ready", nil)
	if err := c.RemoveLabel(ctx, 12, "absent"); err != nil {
		t.Errorf("removing an absent label: %v", err)
	}
	u, err := c.Comment(ctx, 12, "Review posted.\n", "run=4 stage=review --> <script>")
	check("comment", err, "POST", "/repos/showmethebots/demos/issues/12/comments", nil)
	if body := f.last().Body["body"].(string); body != "Review posted.\n\n<!-- loop-verdict run=4 stage=review > <script> -->" {
		t.Errorf("comment body = %q", body)
	}
	if !strings.HasSuffix(u, "issuecomment-1") {
		t.Errorf("comment url = %q", u)
	}
	check("status", c.SetStatus(ctx, "abc123", "pending", strings.Repeat("w", 200), ""), "POST", "/repos/showmethebots/demos/statuses/abc123",
		map[string]any{"state": "pending", "context": "chuck/loop", "description": strings.Repeat("w", 140)})
	if err := c.SetStatus(ctx, "abc123", "approved", "", ""); err == nil {
		t.Error("bad status state sent")
	}
	check("dispatch", c.DispatchWorkflow(ctx, "deploy-prod.yml", "main", map[string]string{"sha": "abc123"}), "POST",
		"/repos/showmethebots/demos/actions/workflows/deploy-prod.yml/dispatches", map[string]any{"ref": "main", "inputs": map[string]string{"sha": "abc123"}})
	pr, err := c.PullRequest(ctx, 13)
	check("pull request", err, "GET", "/repos/showmethebots/demos/pulls/13", nil)
	if pr.Head.SHA != "abc123" || pr.NodeID != "PR_1" || pr.User.ID != 99 {
		t.Errorf("pr = %+v", pr)
	}
	is, err := c.GetIssue(ctx, 12)
	check("issue", err, "GET", "/repos/showmethebots/demos/issues/12", nil)
	if is.Body != "AC-1" || len(is.Labels) != 1 {
		t.Errorf("issue = %+v", is)
	}
	if _, err := c.GetIssue(ctx, 99); !IsStatus(err, 404) {
		t.Errorf("missing issue: %v", err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestPaginatedLists(t *testing.T) {
	f, c := newClient(t)
	page := func(n int, name string) string {
		var out []map[string]any
		for i := range n {
			out = append(out, map[string]any{"filename": name, "previous_filename": "", "id": i, "body": "b", "user": map[string]any{"id": 1}})
		}
		return mustJSON(out)
	}
	f.ok("GET /repos/showmethebots/demos/pulls/13/files?per_page=100&page=1", page(100, "a.go"))
	f.ok("GET /repos/showmethebots/demos/pulls/13/files?per_page=100&page=2", `[{"filename":"db/migrations/2.sql","previous_filename":"db/old.sql","status":"renamed"}]`)
	files, err := c.PRFiles(context.Background(), 13)
	if err != nil || len(files) != 101 || files[100].PreviousName != "db/old.sql" {
		t.Errorf("files = %d, %v", len(files), err)
	}
	for p := 1; p <= 31; p++ {
		f.ok("GET /repos/showmethebots/demos/pulls/14/files?per_page=100&page="+strconv.Itoa(p), page(100, "x"))
	}
	if _, err := c.PRFiles(context.Background(), 14); err == nil {
		t.Error("a PR at GitHub's 3000-file limit was listed as complete")
	}
	f.ok("GET /repos/showmethebots/demos/issues/12/comments?per_page=100&page=1", page(100, ""))
	f.ok("GET /repos/showmethebots/demos/issues/12/comments?per_page=100&page=2", page(3, ""))
	cs, err := c.IssueComments(context.Background(), 12)
	if err != nil || len(cs) != 103 {
		t.Errorf("comments = %d, %v", len(cs), err)
	}
}

func TestAutoMergeFallsBackToMergeWhenRefused(t *testing.T) {
	ctx := context.Background()
	pr := PR{Number: 13, NodeID: "PR_1"}
	for _, c := range []struct {
		name, graphql string
		merged        bool
		wantErr       bool
	}{
		{"accepted", `{"data":{"enablePullRequestAutoMerge":{"clientMutationId":null}}}`, false, false},
		{"refused on a clean PR", `{"errors":[{"message":"Pull request is in clean status"}]}`, true, false},
		{"other error", `{"errors":[{"message":"Auto merge is not allowed for this repository"}]}`, false, true},
	} {
		f, cl := newClient(t)
		f.ok("POST /graphql", c.graphql)
		f.ok("PUT /repos/showmethebots/demos/pulls/13/merge", `{"merged":true}`)
		merged, err := cl.AutoMerge(ctx, pr, "abc123", "SQUASH")
		if merged != c.merged || (err != nil) != c.wantErr {
			t.Errorf("%s: merged %v, err %v", c.name, merged, err)
		}
		var sawMerge bool
		for _, call := range f.calls {
			if call.Path == "/graphql" && call.Body["variables"].(map[string]any)["m"] != "SQUASH" {
				t.Errorf("%s: merge method %v", c.name, call.Body["variables"])
			}
			if call.Method == "PUT" {
				sawMerge = true
				if call.Body["sha"] != "abc123" || call.Body["merge_method"] != "squash" {
					t.Errorf("%s: merge body %v", c.name, call.Body)
				}
			}
		}
		if sawMerge != c.merged {
			t.Errorf("%s: direct merge called = %v", c.name, sawMerge)
		}
	}
	// A head that moved makes GitHub answer 409; that is an error, not a merge.
	f, cl := newClient(t)
	f.ok("POST /graphql", `{"errors":[{"message":"Pull request is in clean status"}]}`)
	f.routes["PUT /repos/showmethebots/demos/pulls/13/merge"] = func(w http.ResponseWriter, _ call) {
		w.WriteHeader(409)
		_, _ = io.WriteString(w, `{"message":"Head branch was modified"}`)
	}
	if merged, err := cl.AutoMerge(ctx, pr, "abc123", "SQUASH"); merged || !IsStatus(err, 409) {
		t.Errorf("moved head: merged %v, err %v", merged, err)
	}
}

func TestReadLoopConfigByBlobSHA(t *testing.T) {
	ctx := context.Background()
	f, c := newClient(t)
	content := `{"wip_limit":1}`
	enc := base64.StdEncoding.EncodeToString([]byte(content))
	f.ok("GET /repos/showmethebots/demos/contents/.claude/loop.json?ref=main",
		`{"type":"file","sha":"b10b","encoding":"base64","content":"`+enc[:4]+`\n`+enc[4:]+`"}`)
	b, err := c.ReadFile(ctx, LoopConfigPath, "main")
	if err != nil || b.SHA != "b10b" || string(b.Content) != content {
		t.Errorf("read = %+v, %v", b, err)
	}
	// Over 1 MB the contents API sends no content; the blob is read by sha.
	f.ok("GET /repos/showmethebots/demos/contents/.claude/loop.json?ref=big", `{"type":"file","sha":"b16","encoding":"none","content":""}`)
	f.ok("GET /repos/showmethebots/demos/git/blobs/b16", `{"sha":"b16","encoding":"base64","content":"`+enc+`"}`)
	if b, err := c.ReadFile(ctx, LoopConfigPath, "big"); err != nil || b.SHA != "b16" || string(b.Content) != content {
		t.Errorf("large read = %+v, %v", b, err)
	}
	f.ok("GET /repos/showmethebots/demos/git/blobs/other", `{"sha":"b16","encoding":"base64","content":"`+enc+`"}`)
	if _, err := c.ReadBlob(ctx, "other"); err == nil {
		t.Error("blob with a different sha accepted")
	}
	if _, err := c.ReadFile(ctx, LoopConfigPath, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
	f.ok("GET /repos/showmethebots/demos/contents/.claude/loop.json?ref=dir", `[{"type":"file"}]`)
	if _, err := c.ReadFile(ctx, LoopConfigPath, "dir"); err == nil {
		t.Error("directory read as a file")
	}
}
