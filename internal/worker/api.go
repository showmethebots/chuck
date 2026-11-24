package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/joho/godotenv"

	"github.com/showmethebots/chuck/internal/config"
	"github.com/showmethebots/chuck/internal/loop"
	"github.com/showmethebots/chuck/internal/memory"
)

// Run API limits.
const (
	// MaxEvidence is the cap on a validate run's evidence tar. Over it the
	// verdict is still accepted; the evidence is dropped and the run says
	// so.
	MaxEvidence = 20 << 20
	// maxVerdictPart bounds the verdict JSON: structured output, usage and
	// a 64 KB log tail.
	maxVerdictPart   = 1 << 20
	maxLogTail       = 64 << 10
	maxEvidenceFiles = 2000
	// maxVerdictBody bounds the whole upload. Evidence past MaxEvidence is
	// never read, so this only caps a client that keeps sending.
	maxVerdictBody = MaxEvidence + 2*maxVerdictPart
	// verdictReadTime replaces the server's 30 s read timeout for the
	// verdict upload, which can carry 20 MB from a slow Machine.
	verdictReadTime = 5 * time.Minute
)

// Spec is what GET /api/runs/{id}/spec returns: everything a worker needs
// that is not its token.
type Spec struct {
	RunID    int64           `json:"run_id"`
	Repo     string          `json:"repo"` // owner/name
	Stage    string          `json:"stage"`
	Attempt  int             `json:"attempt"`
	Issue    int             `json:"issue,omitempty"`
	PR       int             `json:"pr,omitempty"`
	Ref      string          `json:"ref"`            // what to check out
	Base     string          `json:"base"`           // the default branch; build branches from it when Ref does not exist yet
	Push     string          `json:"push,omitempty"` // build and sweep: the branch to push to, also on timeout
	SHA      string          `json:"sha,omitempty"`  // validate: the deployed commit
	Prompt   string          `json:"prompt"`
	Schema   json.RawMessage `json:"schema"`
	Model    string          `json:"model"`
	MaxTurns int             `json:"max_turns"`
	Deadline time.Time       `json:"deadline"`
	// Anthropic is the Claude proxy for this run: the worker sets
	// ANTHROPIC_BASE_URL to it and ANTHROPIC_API_KEY to its run token.
	Anthropic string `json:"anthropic_base_url"`
	// Validate only.
	DevURL string            `json:"dev_url,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
}

// VerdictPost is the "verdict" part of POST /api/runs/{id}/verdict. The
// request is multipart/form-data: the "verdict" part first, then for
// validate an optional "evidence" part holding a tar of evidence/.
type VerdictPost struct {
	// StructuredOutput is claude's --json-schema output. Null when the
	// worker reports an error instead.
	StructuredOutput json.RawMessage `json:"structured_output"`
	// Error is the worker's own failure, "timeout" when it hit its
	// deadline.
	Error      string          `json:"error,omitempty"`
	ExitCode   *int            `json:"exit_code"`
	DurationMS int64           `json:"duration_ms"`
	LogTail    string          `json:"log_tail"`
	Usage      json.RawMessage `json:"usage,omitempty"` // recorded by the Claude proxy, not from here
}

// Handler serves the run API under /api/runs/. Every route authenticates
// with the run's own token as a bearer token, and refuses once the run has
// ended. The Claude proxy (#28) mounts beside these.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/runs/{id}/spec", m.serveSpec)
	mux.HandleFunc("GET /api/runs/{id}/token", m.serveToken)
	mux.HandleFunc("POST /api/runs/{id}/verdict", m.serveVerdict)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// auth finds the run a request's token belongs to. A missing or wrong
// token, or one for another run, is the same 401: the answer never says
// which runs exist.
func (m *Manager) auth(r *http.Request) (memory.Run, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return memory.Run{}, false
	}
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || token == "" {
		return memory.Run{}, false
	}
	run, err := m.Store.RunByToken(r.Context(), token)
	if err != nil || run.ID != id {
		return memory.Run{}, false
	}
	return run, true
}

func (m *Manager) serveSpec(w http.ResponseWriter, r *http.Request) {
	run, ok := m.auth(r)
	if !ok {
		refuse(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	ctx := r.Context()
	switch run.Status {
	case memory.RunLaunching:
		// The first spec fetch is the proof the Machine started: from here
		// on the run uses its attempt up however it ends.
		if err := m.Store.MarkRunRunning(ctx, run.ID, m.now()); err != nil && !errors.Is(err, memory.ErrStale) {
			refuse(w, http.StatusInternalServerError, "could not start the run")
			return
		}
		if run, _ = m.Store.RunByID(ctx, run.ID); run.Status != memory.RunRunning {
			refuse(w, http.StatusUnauthorized, "the run has ended")
			return
		}
	case memory.RunRunning:
	default:
		refuse(w, http.StatusUnauthorized, "the run has ended")
		return
	}
	spec, err := m.spec(ctx, run)
	if err != nil {
		m.log().Error("serve run spec", "run", run.ID, "err", err.Error())
		refuse(w, http.StatusInternalServerError, "could not build the spec")
		return
	}
	writeJSON(w, http.StatusOK, spec)
}

func (m *Manager) runRepo(ctx context.Context, run memory.Run) (memory.Item, memory.Repo, error) {
	it, err := m.Store.ItemByID(ctx, run.ItemID)
	if err != nil {
		return it, memory.Repo{}, err
	}
	repos, err := m.Store.Repos(ctx)
	if err != nil {
		return it, memory.Repo{}, err
	}
	for _, r := range repos {
		if r.ID == it.RepoID {
			return it, r, nil
		}
	}
	return it, memory.Repo{}, memory.ErrNotFound
}

func (m *Manager) spec(ctx context.Context, run memory.Run) (Spec, error) {
	it, repo, err := m.runRepo(ctx, run)
	if err != nil {
		return Spec{}, err
	}
	cfg, err := config.ParseLoop([]byte(repo.ConfigJSON))
	if err != nil {
		return Spec{}, fmt.Errorf("loop.json: %w", err)
	}
	stage := config.Stage(run.Stage)
	schema, err := loop.Schema(stage)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{
		RunID: run.ID, Repo: repo.Owner + "/" + repo.Name, Stage: run.Stage, Attempt: run.Attempt,
		Ref: run.Ref, Base: cfg.DefaultBranch, Prompt: run.Prompt, Schema: schema, Model: run.Model,
		MaxTurns: cfg.MaxTurnsFor(stage), Deadline: run.DeadlineAt,
		Anthropic: strings.TrimRight(m.URL, "/") + fmt.Sprintf("/api/runs/%d/anthropic/", run.ID),
	}
	if it.Kind == memory.KindIssue {
		s.Issue = it.Number
	} else {
		s.PR, s.Issue = it.Number, it.IssueNumber
	}
	switch stage {
	case config.StageBuild, config.StageSweep:
		s.Push = run.Ref
	case config.StageValidate:
		s.SHA, s.DevURL = run.Ref, cfg.Deploy.DevURL
		if s.Env, err = m.validateEnv(repo); err != nil {
			return Spec{}, err
		}
	}
	return s, nil
}

var envNameRe = regexp.MustCompile(`[^A-Z0-9]+`)

// LoopEnvVar is the Fly secret holding a repo's validate credentials, as
// dotenv lines: LOOP_ENV_<OWNER>_<REPO>, upper-cased, with every run of
// other characters as one underscore.
func LoopEnvVar(owner, name string) string {
	return "LOOP_ENV_" + envNameRe.ReplaceAllString(strings.ToUpper(owner), "_") + "_" +
		envNameRe.ReplaceAllString(strings.ToUpper(name), "_")
}

func (m *Manager) validateEnv(repo memory.Repo) (map[string]string, error) {
	if m.Getenv == nil {
		return nil, nil
	}
	v := m.Getenv(LoopEnvVar(repo.Owner, repo.Name))
	if v == "" {
		return nil, nil
	}
	env, err := godotenv.Unmarshal(v)
	if err != nil {
		// Never quote the value: it is a secret.
		return nil, fmt.Errorf("%s is not dotenv lines", LoopEnvVar(repo.Owner, repo.Name))
	}
	return env, nil
}

func (m *Manager) serveToken(w http.ResponseWriter, r *http.Request) {
	run, ok := m.auth(r)
	if !ok {
		refuse(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if run.Status != memory.RunRunning {
		refuse(w, http.StatusUnauthorized, "the run is not running")
		return
	}
	ctx := r.Context()
	_, repo, err := m.runRepo(ctx, run)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "no repo for the run")
		return
	}
	t, err := m.Tokens.WorkerToken(ctx, repo, config.Stage(run.Stage))
	if err != nil {
		m.log().Error("mint worker token", "run", run.ID, "err", err.Error())
		refuse(w, http.StatusBadGateway, "could not mint a token")
		return
	}
	if _, has := t.Permissions["statuses"]; has {
		// github.App.WorkerToken already refuses this; a worker must never
		// be able to set chuck/loop, so check again at the door.
		refuse(w, http.StatusInternalServerError, "token carries statuses")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": t.Value, "expires_at": t.ExpiresAt, "permissions": t.Permissions,
	})
}

func (m *Manager) serveVerdict(w http.ResponseWriter, r *http.Request) {
	run, ok := m.auth(r)
	if !ok {
		refuse(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if run.Status != memory.RunRunning {
		// 409, not 401: the worker's retry loop stops on it. A second post,
		// or one after chuck ended the run, changes nothing.
		refuse(w, http.StatusConflict, "the run has already ended")
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(verdictReadTime))
	r.Body = http.MaxBytesReader(w, r.Body, maxVerdictBody)
	post, files, dropped, err := readVerdict(r, config.Stage(run.Stage) == config.StageValidate)
	if err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}

	v := loop.VerdictInput{RunID: run.ID, Stage: config.Stage(run.Stage), Attempt: run.Attempt}
	end := memory.RunEnd{Callback: true, ExitCode: post.ExitCode, DurationMS: max(post.DurationMS, 0),
		LogTail: tail(post.LogTail, maxLogTail)}
	invalid := ""
	switch {
	case post.Error != "":
		reason := oneLine(post.Error, 200)
		v.Err = "worker reported: " + reason
		end.Status, end.Reason = memory.RunFailed, reason
		if post.Error == "timeout" {
			end.Status, end.Reason = memory.RunTimedOut, "timeout"
		}
	default:
		parsed, perr := loop.ParseVerdict(v.Stage, post.StructuredOutput)
		if perr != nil {
			invalid = oneLine(perr.Error(), 2000)
			v.Err, v.Escalate = "invalid verdict: "+invalid, true
			end.Status, end.Reason = memory.RunFailed, "invalid_verdict"
			break
		}
		v.Verdict = parsed
		end.Status, end.VerdictJSON = memory.RunFinished, string(post.StructuredOutput)
	}
	if dropped {
		end.Reason = strings.TrimPrefix(end.Reason+"; evidence over 20 MB, dropped", "; ")
	}

	// A worker that hangs up mid-commit must not cut the effects list
	// short; the commit and its effects run to the end either way.
	ctx := context.WithoutCancel(r.Context())
	if _, err := m.Verdicts.Verdict(ctx, v, end); err != nil {
		if errors.Is(err, memory.ErrStale) {
			refuse(w, http.StatusConflict, "the run has already ended")
			return
		}
		// The run is still open: the worker's retry can land it.
		m.log().Error("apply verdict", "run", run.ID, "err", err.Error())
		refuse(w, http.StatusServiceUnavailable, "could not apply the verdict; retry")
		return
	}
	if len(files) > 0 {
		if err := m.Store.InsertEvidence(ctx, run.ID, files, m.now()); err != nil {
			m.log().Error("store evidence", "run", run.ID, "err", err.Error())
		}
	}
	if invalid != "" {
		refuse(w, http.StatusUnprocessableEntity, "verdict failed its schema: "+invalid)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "evidence_dropped": dropped})
}

// readVerdict reads the multipart upload: the verdict part, then for
// validate the evidence tar. Evidence past MaxEvidence is not read; the
// tar is dropped whole and dropped is true.
func readVerdict(r *http.Request, wantEvidence bool) (post VerdictPost, files []memory.Evidence, dropped bool, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return post, nil, false, errors.New("want multipart/form-data")
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "verdict" {
		return post, nil, false, errors.New(`the first part must be "verdict"`)
	}
	b, err := io.ReadAll(io.LimitReader(part, maxVerdictPart+1))
	if err != nil {
		return post, nil, false, fmt.Errorf("read verdict: %w", err)
	}
	if len(b) > maxVerdictPart {
		return post, nil, false, errors.New("verdict part over 1 MB")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&post); err != nil {
		return post, nil, false, fmt.Errorf("verdict part: %w", err)
	}
	if post.Error == "" && (len(post.StructuredOutput) == 0 || string(post.StructuredOutput) == "null") {
		return post, nil, false, errors.New("verdict part: want structured_output or error")
	}
	if !wantEvidence {
		return post, nil, false, nil
	}
	part, err = mr.NextPart()
	if errors.Is(err, io.EOF) {
		return post, nil, false, nil
	}
	if err != nil {
		return post, nil, false, fmt.Errorf("read evidence: %w", err)
	}
	if part.FormName() != "evidence" {
		return post, nil, false, fmt.Errorf("unexpected part %q", part.FormName())
	}
	files, dropped, err = readEvidence(part)
	return post, files, dropped, err
}

// readEvidence reads a tar of regular files. Paths are cleaned and must
// stay relative; directories, links and anything under .git are skipped.
func readEvidence(rd io.Reader) ([]memory.Evidence, bool, error) {
	lr := &io.LimitedReader{R: rd, N: MaxEvidence + 1}
	tr := tar.NewReader(lr)
	var out []memory.Evidence
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if lr.N <= 0 {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("evidence tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		p, ok := cleanEvidencePath(h.Name)
		if !ok || seen[p] {
			continue
		}
		b, err := io.ReadAll(tr)
		if lr.N <= 0 {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("evidence tar: %w", err)
		}
		if len(out) == maxEvidenceFiles {
			return nil, false, fmt.Errorf("evidence tar: more than %d files", maxEvidenceFiles)
		}
		seen[p] = true
		out = append(out, memory.Evidence{Path: p, ContentType: mime.TypeByExtension(path.Ext(p)), Content: b})
	}
	if lr.N <= 0 {
		return nil, true, nil
	}
	return out, false, nil
}

func cleanEvidencePath(name string) (string, bool) {
	if !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\\") {
		return "", false
	}
	p := path.Clean(strings.TrimPrefix(name, "./"))
	p = strings.TrimPrefix(p, "evidence/")
	if p == "." || p == "evidence" || p == "" || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") ||
		p == ".git" || strings.HasPrefix(p, ".git/") || strings.Contains(p, "/.git/") || len(p) > 512 {
		return "", false
	}
	return p, true
}

// tail keeps the last n bytes of s, cut at a rune boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return strings.ToValidUTF8(s, "�")
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return strings.ToValidUTF8(s, "�")
}

// oneLine flattens and caps text a worker sent before it is recorded as a
// reason. The engine's mention quotes and defuses reasons it posts.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "")
	}
	return s
}
