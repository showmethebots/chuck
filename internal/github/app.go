// Package github is chuck's side of GitHub: App authentication, the REST
// calls the loop makes, and the webhook receiver.
//
// Every installation token is minted with an explicit permission set and
// the one repository it is for, never the App's full grant. Worker tokens
// can never carry `statuses`, so no worker can set `chuck/loop`.
package github

import (
	"bytes"
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
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/showmethebots/chuck/internal/config"
)

// DefaultBaseURL is GitHub's REST API.
const DefaultBaseURL = "https://api.github.com"

// Permissions is an installation token's permission set, as GitHub names
// them: {"contents": "write", "metadata": "read"}.
type Permissions map[string]string

// ChuckPermissions is what chuck's own calls use: labels, comments, the
// `chuck/loop` status, merge, workflow dispatch, and reads.
var ChuckPermissions = Permissions{
	"contents":      "write",
	"issues":        "write",
	"pull_requests": "write",
	"statuses":      "write",
	"actions":       "write",
	"checks":        "read",
	"metadata":      "read",
}

func (p Permissions) key() string {
	ks := make([]string, 0, len(p))
	for k, v := range p {
		ks = append(ks, k+"="+v)
	}
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

// StagePermissions is the one permission set a worker token may carry for
// each stage, from the spec's "Identities". No set names `statuses`, so no
// worker can set `chuck/loop`, nor `workflows`, `administration` or
// `actions`. Intake edits the issue and opens sub-issues.
var StagePermissions = map[config.Stage]Permissions{
	config.StageIntake:   {"issues": "write", "contents": "read", "metadata": "read"},
	config.StageBuild:    {"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"},
	config.StageReview:   {"pull_requests": "write", "contents": "read", "metadata": "read"},
	config.StageSweep:    {"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"},
	config.StageValidate: {"contents": "read", "pull_requests": "read", "issues": "read", "metadata": "read"},
}

// App is one GitHub App: its id and private key.
type App struct {
	ID      int64
	Key     *rsa.PrivateKey
	BaseURL string       // "" means DefaultBaseURL
	HTTP    *http.Client // nil means a client with a 30 s timeout
	Now     func() time.Time

	mu     sync.Mutex
	tokens map[string]Token // chuck's own tokens, by installation and permissions
}

// ParsePrivateKey reads the PEM key GitHub issues (PKCS#1), or PKCS#8.
func ParsePrivateKey(pemKey []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return nil, errors.New("github app key: not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github app key: not an RSA private key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app key: not an RSA private key")
	}
	return rk, nil
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *App) baseURL() string {
	if a.BaseURL != "" {
		return strings.TrimRight(a.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (a *App) http() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// JWT is the App's own credential, valid for nine minutes. It is backdated
// a minute for clock drift, as GitHub recommends.
func (a *App) JWT() (string, error) {
	if a.Key == nil {
		return "", errors.New("github app: no private key")
	}
	now := a.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": fmt.Sprint(a.ID),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Token is an installation token.
type Token struct {
	Value       string      `json:"token"`
	ExpiresAt   time.Time   `json:"expires_at"`
	Permissions Permissions `json:"permissions"`
}

// WorkerToken mints a token for one worker run: the one repository and
// the stage's permission set from StagePermissions, never more. It is
// never cached, and one that comes back carrying a permission the set
// does not name is refused.
func (a *App) WorkerToken(ctx context.Context, installationID int64, repo string, stage config.Stage) (Token, error) {
	perms, ok := StagePermissions[stage]
	if !ok {
		return Token{}, fmt.Errorf("github app: no worker permissions for stage %q", stage)
	}
	t, err := a.mint(ctx, installationID, repo, perms)
	if err != nil {
		return Token{}, err
	}
	for k, v := range t.Permissions {
		if want, ok := perms[k]; !ok || (v == "write" && want != "write") {
			return Token{}, fmt.Errorf("mint worker token: GitHub granted %s:%s, which the %s set does not name", k, v, stage)
		}
	}
	return t, nil
}

// token is chuck's own token for a repository, reused until five minutes
// before it expires.
func (a *App) token(ctx context.Context, installationID int64, repo string, perms Permissions) (string, error) {
	k := fmt.Sprintf("%d/%s/%s", installationID, repo, perms.key())
	a.mu.Lock()
	t, ok := a.tokens[k]
	a.mu.Unlock()
	if ok && a.now().Before(t.ExpiresAt.Add(-5*time.Minute)) {
		return t.Value, nil
	}
	t, err := a.mint(ctx, installationID, repo, perms)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[string]Token{}
	}
	a.tokens[k] = t
	a.mu.Unlock()
	return t.Value, nil
}

func (a *App) mint(ctx context.Context, installationID int64, repo string, perms Permissions) (Token, error) {
	if len(perms) == 0 {
		// An empty set would mean "everything the App has".
		return Token{}, errors.New("github app: a token needs an explicit permission set")
	}
	if repo == "" || strings.Contains(repo, "/") {
		return Token{}, fmt.Errorf("github app: token repository must be a bare name, got %q", repo)
	}
	jwt, err := a.JWT()
	if err != nil {
		return Token{}, err
	}
	body, err := json.Marshal(map[string]any{"repositories": []string{repo}, "permissions": perms})
	if err != nil {
		return Token{}, err
	}
	var t Token
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := a.do(ctx, "Bearer "+jwt, http.MethodPost, path, body, &t); err != nil {
		return Token{}, fmt.Errorf("mint installation token: %w", err)
	}
	// GitHub silently drops a permission the App was not granted; a token
	// that came back with less than asked for is refused rather than used.
	for k, v := range perms {
		if got := t.Permissions[k]; got != v && (v != "read" || got != "write") {
			return Token{}, fmt.Errorf("mint installation token: asked for %s:%s, got %q", k, v, got)
		}
	}
	return t, nil
}

// RevokeToken revokes an installation token, authenticated as the token
// itself. A token already expired or revoked answers 401, which is success
// here: either way it no longer works. baseURL "" means DefaultBaseURL.
func RevokeToken(ctx context.Context, hc *http.Client, baseURL, token string) error {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	a := &App{BaseURL: baseURL, HTTP: hc}
	err := a.do(ctx, "token "+token, http.MethodDelete, "/installation/token", nil, nil)
	if IsStatus(err, http.StatusUnauthorized) {
		return nil
	}
	return err
}

// APIError is a non-2xx answer from GitHub.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsStatus reports whether err is an APIError with that HTTP status.
func IsStatus(err error, status int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == status
}

func (a *App) do(ctx context.Context, auth, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("User-Agent", "chuck")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &m)
		if m.Message == "" {
			m.Message = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: m.Message}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("github %s %s: %w", method, path, err)
		}
	}
	return nil
}
