package config

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// ChuckFile is chuck-wide config's name inside the config directory.
const ChuckFile = "chuck.yaml"

// Chuck is config/chuck.yaml. String fields may hold ${VAR} references,
// which stay unresolved until Resolve: `chuck validate` checks the file in
// CI, where none of the secrets exist.
type Chuck struct {
	Humans           []Human  `yaml:"humans"`
	Apps             []App    `yaml:"apps"`
	Workers          Workers  `yaml:"workers"`
	Spend            Spend    `yaml:"spend"`
	Tick             Duration `yaml:"tick"`
	WebhookRetention Duration `yaml:"webhook_retention"`
}

// Human is a GitHub user chuck trusts: dashboard login, labels that start
// or approve stages, feedback that reaches a prompt. The numeric id is the
// identity; logins can be renamed and reused.
type Human struct {
	ID int64 `yaml:"id"`
}

// App is one GitHub App chuck acts as. AppID is a string so it can be a
// ${VAR}; it must resolve to a positive integer.
type App struct {
	Owner             string `yaml:"owner"`
	AppID             string `yaml:"app_id"`
	PrivateKey        string `yaml:"private_key"`
	OAuthClientID     string `yaml:"oauth_client_id"`
	OAuthClientSecret string `yaml:"oauth_client_secret"`
}

// Workers says where stage workers run.
type Workers struct {
	FlyApp           string   `yaml:"fly_app"`
	Image            string   `yaml:"image"`
	WallClockCeiling Duration `yaml:"wall_clock_ceiling"`
}

// Spend caps Claude spend per calendar month, in US dollars.
type Spend struct {
	MonthlyUSD        float64 `yaml:"monthly_usd"`
	PerRepoMonthlyUSD float64 `yaml:"per_repo_monthly_usd"`
}

// IsHuman reports whether id is in humans.
func (c *Chuck) IsHuman(id int64) bool {
	for _, h := range c.Humans {
		if h.ID == id {
			return true
		}
	}
	return false
}

// LoadChuck reads and checks dir/chuck.yaml without resolving references.
func LoadChuck(dir string) (*Chuck, error) {
	b, err := os.ReadFile(filepath.Join(dir, ChuckFile))
	if err != nil {
		return nil, err
	}
	return ParseChuck(b)
}

// ParseChuck decodes chuck.yaml strictly and reports every structural
// problem at once. References are checked for syntax, not resolved.
func ParseChuck(b []byte) (*Chuck, error) {
	var c Chuck
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", ChuckFile, err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Chuck) validate() error {
	var p problems
	if len(c.Humans) == 0 {
		p.add("humans: at least one is required")
	}
	seenHuman := map[int64]bool{}
	for i, h := range c.Humans {
		if h.ID <= 0 {
			p.add("humans[%d].id: want a GitHub numeric user id, got %d", i, h.ID)
		}
		if seenHuman[h.ID] {
			p.add("humans[%d].id: %d is listed twice", i, h.ID)
		}
		seenHuman[h.ID] = true
	}

	if len(c.Apps) == 0 {
		p.add("apps: at least one is required")
	}
	seenOwner := map[string]bool{}
	for i, a := range c.Apps {
		at := fmt.Sprintf("apps[%d]", i)
		if a.Owner == "" {
			p.add("%s.owner: required", at)
		}
		if seenOwner[a.Owner] {
			p.add("%s.owner: %q is listed twice", at, a.Owner)
		}
		seenOwner[a.Owner] = true
		checkValue(&p, at+".app_id", a.AppID, false)
		if a.AppID != "" && !hasRef(a.AppID) {
			checkAppID(&p, at+".app_id", a.AppID)
		}
		checkValue(&p, at+".oauth_client_id", a.OAuthClientID, false)
		// A secret written into the file would be committed and baked into
		// the image, so these two may only name an environment variable.
		checkValue(&p, at+".private_key", a.PrivateKey, true)
		checkValue(&p, at+".oauth_client_secret", a.OAuthClientSecret, true)
	}

	checkValue(&p, "workers.fly_app", c.Workers.FlyApp, false)
	checkValue(&p, "workers.image", c.Workers.Image, false)
	if c.Workers.WallClockCeiling <= 0 {
		p.add("workers.wall_clock_ceiling: required, above zero")
	}

	if c.Spend.MonthlyUSD <= 0 {
		p.add("spend.monthly_usd: required, above zero; there is no unlimited")
	}
	if c.Spend.PerRepoMonthlyUSD <= 0 {
		p.add("spend.per_repo_monthly_usd: required, above zero; there is no unlimited")
	}
	if c.Spend.PerRepoMonthlyUSD > c.Spend.MonthlyUSD && c.Spend.MonthlyUSD > 0 {
		p.add("spend.per_repo_monthly_usd: %v is above spend.monthly_usd %v", c.Spend.PerRepoMonthlyUSD, c.Spend.MonthlyUSD)
	}
	if c.Tick < Duration(time.Minute) {
		p.add("tick: required, at least 1m")
	}
	if c.WebhookRetention < Duration(24*time.Hour) {
		p.add("webhook_retention: required, at least 24h")
	}
	return p.err()
}

var (
	refRe      = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`)
	wholeRefRe = regexp.MustCompile(`^\$\{[A-Z][A-Z0-9_]*\}$`)
)

func hasRef(s string) bool { return refRe.MatchString(s) }

// checkValue requires a value, rejects a "$" that is not part of a valid
// ${VAR} (a typo there would otherwise be used literally), and when
// refOnly, requires the whole value to be one reference.
func checkValue(p *problems, at, v string, refOnly bool) {
	if v == "" {
		p.add("%s: required", at)
		return
	}
	if strings.Contains(refRe.ReplaceAllString(v, ""), "$") {
		p.add("%s: %q has a \"$\" that is not a ${VAR} reference", at, v)
	}
	if refOnly && !wholeRefRe.MatchString(v) {
		p.add("%s: must be a ${VAR} reference to an environment variable, never the value itself", at)
	}
}

func checkAppID(p *problems, at, v string) {
	if n, err := strconv.ParseInt(v, 10, 64); err != nil || n <= 0 {
		p.add("%s: want a positive integer, got %q", at, v)
	}
}

// Resolve returns a copy of c with every ${VAR} replaced from getenv. An
// unset or empty variable is an error, and every one is reported. The
// resolved values are checked too: app_id is a positive integer and
// private_key is a PEM private key.
func (c *Chuck) Resolve(getenv func(string) string) (*Chuck, error) {
	var p problems
	sub := func(at, v string) string {
		return refRe.ReplaceAllStringFunc(v, func(ref string) string {
			name := refRe.FindStringSubmatch(ref)[1]
			val := getenv(name)
			if val == "" {
				p.add("%s: environment variable %s is not set", at, name)
			}
			return val
		})
	}
	r := *c
	r.Apps = make([]App, len(c.Apps))
	for i, a := range c.Apps {
		at := fmt.Sprintf("apps[%d]", i)
		r.Apps[i] = App{
			Owner:             sub(at+".owner", a.Owner),
			AppID:             sub(at+".app_id", a.AppID),
			PrivateKey:        sub(at+".private_key", a.PrivateKey),
			OAuthClientID:     sub(at+".oauth_client_id", a.OAuthClientID),
			OAuthClientSecret: sub(at+".oauth_client_secret", a.OAuthClientSecret),
		}
		if r.Apps[i].AppID != "" {
			checkAppID(&p, at+".app_id", r.Apps[i].AppID)
		}
		if r.Apps[i].PrivateKey != "" && !isPrivateKey(r.Apps[i].PrivateKey) {
			// Never quote the value: it is a secret.
			p.add("%s.private_key: not a PEM-encoded private key", at)
		}
	}
	r.Humans = append([]Human(nil), c.Humans...)
	r.Workers.FlyApp = sub("workers.fly_app", c.Workers.FlyApp)
	r.Workers.Image = sub("workers.image", c.Workers.Image)
	if err := p.err(); err != nil {
		return nil, err
	}
	return &r, nil
}

func isPrivateKey(s string) bool {
	blk, _ := pem.Decode([]byte(s))
	if blk == nil {
		return false
	}
	if _, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return true
	}
	_, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	return err == nil
}
