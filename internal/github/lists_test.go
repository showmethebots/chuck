package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestListsFollowPagesAndRefuseRunaways(t *testing.T) {
	f, c := newClient(t)
	ctx := context.Background()
	page := func(n, from int) string {
		var rs []string
		for i := 0; i < n; i++ {
			rs = append(rs, fmt.Sprintf(`{"name":"c%d","status":"completed","conclusion":"success"}`, from+i))
		}
		return `{"check_runs":[` + strings.Join(rs, ",") + `]}`
	}
	f.ok("GET /repos/showmethebots/demos/commits/abc/check-runs?per_page=100&page=1", page(100, 0))
	f.ok("GET /repos/showmethebots/demos/commits/abc/check-runs?per_page=100&page=2", page(3, 100))
	runs, err := c.CheckRuns(ctx, "abc")
	if err != nil || len(runs) != 103 {
		t.Fatalf("CheckRuns = %d, %v; want 103", len(runs), err)
	}

	for p := 1; p <= maxPages; p++ {
		var ss []string
		for i := 0; i < 100; i++ {
			ss = append(ss, `{"state":"success","context":"ci"}`)
		}
		f.ok(fmt.Sprintf("GET /repos/showmethebots/demos/commits/def/statuses?per_page=100&page=%d", p), "["+strings.Join(ss, ",")+"]")
	}
	if _, err := c.Statuses(ctx, "def"); err == nil {
		t.Error("a list longer than the page cap was returned as complete")
	}
}

func TestSubIssuesReviewsEventsRuns(t *testing.T) {
	evAt := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	f, c := newClient(t)
	ctx := context.Background()
	f.ok("GET /repos/showmethebots/demos/issues/12/sub_issues?per_page=100&page=1", `[{"number":14},{"number":15}]`)
	f.ok("GET /repos/showmethebots/demos/pulls/13/reviews?per_page=100&page=1",
		`[{"id":1,"user":{"id":5,"login":"h"},"state":"APPROVED","commit_id":"abc"}]`)
	f.ok("GET /repos/showmethebots/demos/issues/12/events?per_page=100&page=1",
		`[{"id":9,"event":"labeled","actor":{"id":5},"created_at":"2026-11-20T12:00:00Z","label":{"name":"loop:intake"}}]`)
	f.ok("GET /repos/showmethebots/demos/actions/runs?head_sha=abc&per_page=100&page=1",
		`{"workflow_runs":[{"id":3,"path":".github/workflows/deploy-dev.yml","head_sha":"abc","event":"push","head_repository":{"id":1},"repository":{"id":1}}]}`)
	f.ok("GET /repos/showmethebots/demos/actions/workflows/deploy-prod.yml/runs?event=workflow_dispatch&created=%3E%3D2026-11-20T12%3A00%3A00Z&per_page=100&page=1",
		`{"workflow_runs":[{"id":4,"path":".github/workflows/deploy-prod.yml","display_title":"Deploy abc","event":"workflow_dispatch","created_at":"2026-11-20T12:01:00Z"}]}`)
	f.ok("GET /repos/showmethebots/demos/issues?state=open&labels=loop%3Aintake&per_page=100&page=1", `[{"number":12}]`)
	f.ok("GET /user/5", `{"id":5,"login":"h","type":"User"}`)
	f.ok("GET /repos/showmethebots/demos/pulls/13/comments?per_page=100&page=1",
		`[{"id":2,"user":{"id":5,"login":"h"},"body":"fix this","path":"a.go","html_url":"https://x/c2"}]`)

	if cs, err := c.ReviewComments(ctx, 13); err != nil || len(cs) != 1 || cs[0].Body != "fix this" || cs[0].User.ID != 5 || cs[0].HTMLURL != "https://x/c2" {
		t.Errorf("ReviewComments = %+v, %v", cs, err)
	}
	if subs, err := c.SubIssues(ctx, 12); err != nil || fmt.Sprint(subs) != "[14 15]" {
		t.Errorf("SubIssues = %v, %v", subs, err)
	}
	if rs, err := c.Reviews(ctx, 13); err != nil || len(rs) != 1 || rs[0].State != "APPROVED" || rs[0].CommitID != "abc" || rs[0].User.ID != 5 {
		t.Errorf("Reviews = %+v, %v", rs, err)
	}
	if es, err := c.IssueEvents(ctx, 12); err != nil || len(es) != 1 || es[0].Label.Name != "loop:intake" || es[0].Actor.ID != 5 || !es[0].CreatedAt.Equal(evAt) {
		t.Errorf("IssueEvents = %+v, %v", es, err)
	}
	if runs, err := c.WorkflowRuns(ctx, "abc"); err != nil || len(runs) != 1 || runs[0].Path != ".github/workflows/deploy-dev.yml" {
		t.Errorf("WorkflowRuns = %+v, %v", runs, err)
	}
	if runs, err := c.DispatchRuns(ctx, "deploy-prod.yml", evAt); err != nil || len(runs) != 1 || runs[0].DisplayTitle != "Deploy abc" {
		t.Errorf("DispatchRuns = %+v, %v", runs, err)
	}
	if is, err := c.OpenIssuesWithLabel(ctx, "loop:intake"); err != nil || len(is) != 1 {
		t.Errorf("OpenIssuesWithLabel = %+v, %v", is, err)
	}
	if u, err := c.UserByID(ctx, 5); err != nil || u.Login != "h" {
		t.Errorf("UserByID = %+v, %v", u, err)
	}
}

func TestMarkerTextMatchesComment(t *testing.T) {
	f, c := newClient(t)
	var body string
	f.routes["POST /repos/showmethebots/demos/issues/12/comments"] = func(w http.ResponseWriter, call call) {
		body = call.Body["body"].(string)
		_, _ = io.WriteString(w, `{"html_url":"u"}`)
	}
	if _, err := c.Comment(context.Background(), 12, "hi", "transition=4 effect=1"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(body, MarkerText("transition=4 effect=1")) {
		t.Errorf("body %q does not end in the marker", body)
	}
}

func TestDiscoveryTokenIsMetadataOnly(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	var asked map[string]any
	f.routes["POST /app/installations/7/access_tokens"] = func(w http.ResponseWriter, c call) {
		asked = c.Body
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_d", "expires_at": time.Now().Add(time.Hour),
			"permissions": map[string]string{"metadata": "read"}})
	}
	f.ok("GET /installation/repositories?per_page=100&page=1",
		`{"repositories":[{"id":1,"name":"demos","full_name":"showmethebots/demos","owner":{"login":"showmethebots"},"default_branch":"main"}]}`)
	repos, err := a.InstallationRepos(ctx, 7)
	if err != nil || len(repos) != 1 || repos[0].DefaultBranch != "main" {
		t.Fatalf("InstallationRepos = %+v, %v", repos, err)
	}
	if _, ok := asked["repositories"]; ok {
		t.Error("discovery token was scoped to repositories; it lists them")
	}
	if p, _ := json.Marshal(asked["permissions"]); string(p) != `{"metadata":"read"}` {
		t.Errorf("discovery token asked for %s; want metadata read only", p)
	}
	if got := f.last().Auth; got != "token ghs_d" {
		t.Errorf("listed with %q", got)
	}

	// A grant wider than asked is refused, not used.
	f.routes["POST /app/installations/7/access_tokens"] = func(w http.ResponseWriter, c call) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_d", "expires_at": time.Now().Add(time.Hour),
			"permissions": map[string]string{"metadata": "read", "contents": "write"}})
	}
	if _, err := a.InstallationRepos(ctx, 7); err == nil {
		t.Error("a discovery token carrying contents write was used")
	}
	if _, err := a.mintAll(ctx, 7, Permissions{"contents": "read"}); err == nil {
		t.Error("mintAll minted an unscoped token beyond metadata read")
	}
}

func TestInstallationsAndBot(t *testing.T) {
	f, a := newFake(t)
	ctx := context.Background()
	f.ok("GET /app/installations?per_page=100&page=1", `[{"id":7,"account":{"login":"showmethebots"}}]`)
	f.ok("GET /app", `{"slug":"chuck-showme-bot"}`)
	f.ok("GET /users/chuck-showme-bot%5Bbot%5D", `{"id":9000,"login":"chuck-showme-bot[bot]","type":"Bot"}`)
	ins, err := a.Installations(ctx)
	if err != nil || len(ins) != 1 || ins[0].ID != 7 {
		t.Fatalf("Installations = %+v, %v", ins, err)
	}
	if !strings.HasPrefix(f.last().Auth, "Bearer ") {
		t.Errorf("installations listed with %q; want the App JWT", f.last().Auth)
	}
	bot, err := a.Bot(ctx)
	if err != nil || bot.ID != 9000 {
		t.Fatalf("Bot = %+v, %v", bot, err)
	}
	if got := f.last().Auth; got != "" {
		t.Errorf("public user lookup sent Authorization %q", got)
	}
	f.ok("GET /users/chuck-showme-bot%5Bbot%5D", `{"id":9000,"login":"chuck-showme-bot","type":"User"}`)
	if _, err := a.Bot(ctx); err == nil {
		t.Error("a non-bot account was taken as the App's bot")
	}
}

func TestPRFilesTooMany(t *testing.T) {
	f, c := newClient(t)
	var files []string
	for i := 0; i < 100; i++ {
		files = append(files, fmt.Sprintf(`{"filename":"f%d"}`, i))
	}
	for p := 1; p <= 30; p++ {
		f.ok(fmt.Sprintf("GET /repos/showmethebots/demos/pulls/13/files?per_page=100&page=%d", p), "["+strings.Join(files, ",")+"]")
	}
	if _, err := c.PRFiles(context.Background(), 13); !errors.Is(err, ErrTooManyFiles) {
		t.Errorf("err = %v; want ErrTooManyFiles", err)
	}
}
