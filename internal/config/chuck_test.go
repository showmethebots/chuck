package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

const validChuck = `
humans:
  - id: 1234567
apps:
  - owner: orionsbelt-dev
    app_id: ${GITHUB_APP_ID}
    private_key: ${GITHUB_APP_PRIVATE_KEY}
    oauth_client_id: ${GITHUB_OAUTH_CLIENT_ID}
    oauth_client_secret: ${GITHUB_OAUTH_CLIENT_SECRET}
workers:
  fly_app: chuck-workers
  image: registry.fly.io/chuck-workers:sha-abc
  wall_clock_ceiling: 4h
spend:
  monthly_usd: 100
  per_repo_monthly_usd: 60
tick: 30m
webhook_retention: 720h
`

func testKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestShippedChuckConfigParses(t *testing.T) {
	if _, err := LoadChuck("../../config"); err != nil {
		t.Fatal(err)
	}
}

func TestParseChuckValid(t *testing.T) {
	c, err := ParseChuck([]byte(validChuck))
	if err != nil {
		t.Fatal(err)
	}
	if !c.IsHuman(1234567) || c.IsHuman(1) {
		t.Errorf("IsHuman wrong")
	}
	if c.Tick.Std().Minutes() != 30 {
		t.Errorf("tick = %v", c.Tick)
	}
}

func TestParseChuckUnknownField(t *testing.T) {
	_, err := ParseChuck([]byte(validChuck + "slack: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "slack") {
		t.Fatalf("err = %v, want unknown field slack", err)
	}
}

func TestParseChuckReportsEveryProblem(t *testing.T) {
	bad := `
humans:
  - id: 0
  - id: 5
  - id: 5
apps:
  - owner: ""
    app_id: "12x"
    private_key: "-----BEGIN RSA PRIVATE KEY-----"
    oauth_client_id: "$OOPS"
    oauth_client_secret: "plain-secret"
workers:
  fly_app: ""
  image: ""
  wall_clock_ceiling: 0s
spend:
  monthly_usd: 50
  per_repo_monthly_usd: 60
tick: 10s
webhook_retention: 1h
`
	_, err := ParseChuck([]byte(bad))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"humans[0].id", "humans[2].id: 5 is listed twice", "apps[0].owner",
		"apps[0].app_id: want a positive integer", "apps[0].private_key: must be a ${VAR}",
		"apps[0].oauth_client_id: \"$OOPS\"", "apps[0].oauth_client_secret: must be a ${VAR}",
		"workers.fly_app", "workers.image", "workers.wall_clock_ceiling",
		"per_repo_monthly_usd: 60 is above", "tick", "webhook_retention",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	// The pasted key must not be echoed back.
	if strings.Contains(err.Error(), "BEGIN RSA") || strings.Contains(err.Error(), "plain-secret") {
		t.Errorf("error quotes a secret value:\n%v", err)
	}
}

func TestParseChuckRequiresHumansAppsAndCaps(t *testing.T) {
	_, err := ParseChuck([]byte("workers: {fly_app: a, image: b, wall_clock_ceiling: 1h}\ntick: 1m\nwebhook_retention: 24h\n"))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"humans: at least one", "apps: at least one", "spend.monthly_usd", "spend.per_repo_monthly_usd"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestResolve(t *testing.T) {
	c, err := ParseChuck([]byte(validChuck))
	if err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	r, err := c.Resolve(envMap(map[string]string{
		"GITHUB_APP_ID": "42", "GITHUB_APP_PRIVATE_KEY": key,
		"GITHUB_OAUTH_CLIENT_ID": "Iv1.abc", "GITHUB_OAUTH_CLIENT_SECRET": "s3cret",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if r.Apps[0].AppID != "42" || r.Apps[0].PrivateKey != key || r.Apps[0].OAuthClientSecret != "s3cret" {
		t.Errorf("resolved = %+v", r.Apps[0])
	}
	if c.Apps[0].AppID != "${GITHUB_APP_ID}" {
		t.Errorf("Resolve changed the original: %q", c.Apps[0].AppID)
	}
}

func TestResolveReportsEveryUnsetVar(t *testing.T) {
	c, err := ParseChuck([]byte(validChuck))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Resolve(envMap(nil))
	if err == nil {
		t.Fatal("no error")
	}
	for _, v := range []string{"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY", "GITHUB_OAUTH_CLIENT_ID", "GITHUB_OAUTH_CLIENT_SECRET"} {
		if !strings.Contains(err.Error(), v+" is not set") {
			t.Errorf("error does not name %s:\n%v", v, err)
		}
	}
}

func TestResolveChecksValues(t *testing.T) {
	c, err := ParseChuck([]byte(validChuck))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Resolve(envMap(map[string]string{
		"GITHUB_APP_ID": "abc", "GITHUB_APP_PRIVATE_KEY": "not a key",
		"GITHUB_OAUTH_CLIENT_ID": "x", "GITHUB_OAUTH_CLIENT_SECRET": "y",
	}))
	if err == nil || !strings.Contains(err.Error(), "app_id: want a positive integer") ||
		!strings.Contains(err.Error(), "private_key: not a PEM-encoded private key") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "not a key\"") {
		t.Errorf("error quotes the private key value: %v", err)
	}
}

// YAML accepts .nan and .inf; a cap that compares false against everything
// would be no cap at all.
func TestSpendCapsRejectNaNAndInf(t *testing.T) {
	for _, v := range []string{".nan", ".inf"} {
		y := strings.Replace(validChuck, "monthly_usd: 100", "monthly_usd: "+v, 1)
		y = strings.Replace(y, "per_repo_monthly_usd: 60", "per_repo_monthly_usd: "+v, 1)
		_, err := ParseChuck([]byte(y))
		if err == nil || !strings.Contains(err.Error(), "spend.monthly_usd") || !strings.Contains(err.Error(), "spend.per_repo_monthly_usd") {
			t.Errorf("%s: err = %v", v, err)
		}
	}
}
