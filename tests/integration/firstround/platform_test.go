//go:build integration && acceptance

package firstround

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/juano-morello/orvex/internal/audit/chain"
	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/canonjson"
	"github.com/juano-morello/orvex/internal/persistence/postgres/pgtest"
)

// platform is one empty database migrated by crashctl, the configuration
// files the services read, and the services the test starts.
type platform struct {
	t   *testing.T
	bin binaries
	obs observer

	ownerDSN  string
	engineDSN string

	keyringFile, tokenFile, playersFile string
	tokens                              map[string]string

	secrets  []string
	services []*service
	cli      []string
}

// newPlatform migrates a fresh empty database with crashctl migrate up and
// writes the keyring, the engine/gateway shared token and a players file
// with a random bearer token per player. The engine connects as its own
// login role, a member of the schema owner, so its password is one no log
// may contain.
func newPlatform(t *testing.T, players ...string) *platform {
	t.Helper()
	p := &platform{t: t, bin: builtBinaries(t), tokens: map[string]string{}}
	requireVersion(t, p.bin.engine, "engine")
	requireVersion(t, p.bin.gateway, "gateway")
	requireVersion(t, p.bin.crashctl, "crashctl")

	p.ownerDSN = pgtest.EmptyDatabase(t)
	p.mustCrashctl(nil, "migrate", "up")
	p.obs = observer{t: t, db: pgtest.Pool(t, p.ownerDSN)}

	enginePassword := randomHex(t)
	p.engineDSN = p.loginRole("orvex_acceptance_engine", enginePassword, `IN ROLE `+p.ownerRole())
	p.secrets = append(p.secrets, enginePassword)

	dir := t.TempDir()
	p.keyringFile = pgtest.KeyringFile(t)
	keyring, err := os.ReadFile(p.keyringFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(keyring)), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "key" {
			p.secrets = append(p.secrets, f[2])
		}
	}

	gatewayToken := randomHex(t)
	p.tokenFile = writeFile(t, dir, "gateway-token", gatewayToken+"\n")
	p.secrets = append(p.secrets, gatewayToken)

	byToken := map[string]string{}
	for _, name := range players {
		tok := randomHex(t)
		p.tokens[name] = tok
		byToken[tok] = name
		p.secrets = append(p.secrets, tok)
	}
	b, err := json.Marshal(byToken)
	if err != nil {
		t.Fatal(err)
	}
	p.playersFile = writeFile(t, dir, "players.json", string(b))
	return p
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (p *platform) ownerRole() string {
	u, err := url.Parse(p.ownerDSN)
	if err != nil {
		p.t.Fatal(err)
	}
	return pgx.Identifier{u.User.Username()}.Sanitize()
}

// loginRole creates a login role <prefix>_<random> with password and the
// given CREATE ROLE options, drops it at cleanup, and returns the owner
// DSN with its credentials.
func (p *platform) loginRole(prefix, password, options string, grants ...string) string {
	p.t.Helper()
	ctx := context.Background()
	role := prefix + "_" + randomHex(p.t)[:12]
	quoted := pgx.Identifier{role}.Sanitize()
	db := p.obs.db
	if _, err := db.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' %s`, quoted, password, options)); err != nil {
		p.t.Fatalf("create role %s: %v", role, err)
	}
	p.t.Cleanup(func() {
		for _, sql := range []string{`DROP OWNED BY ` + quoted, `DROP ROLE ` + quoted} {
			if _, err := db.Exec(ctx, sql); err != nil {
				p.t.Errorf("%s: %v", sql, err)
			}
		}
	})
	var database string
	if err := db.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		p.t.Fatal(err)
	}
	for _, g := range grants {
		if _, err := db.Exec(ctx, fmt.Sprintf(g, quoted, pgx.Identifier{database}.Sanitize())); err != nil {
			p.t.Fatalf("%s: %v", g, err)
		}
	}
	u, err := url.Parse(p.ownerDSN)
	if err != nil {
		p.t.Fatal(err)
	}
	u.User = url.UserPassword(role, password)
	return u.String()
}

type cliResult struct {
	code           int
	stdout, stderr string
}

// crashctl runs crashctl with ORVEX_DATABASE_URL set to the owner DSN, or
// with exactly env when env is not nil.
func (p *platform) crashctl(env []string, args ...string) cliResult {
	p.t.Helper()
	if env == nil {
		env = []string{"ORVEX_DATABASE_URL=" + p.ownerDSN}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.bin.crashctl, args...) //nolint:gosec // G204: crashctl is a binary this test built
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := cliResult{}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			p.t.Fatalf("crashctl %s: %v", strings.Join(args, " "), err)
		}
		res.code = exitErr.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	p.cli = append(p.cli, res.stdout, res.stderr)
	return res
}

func (p *platform) mustCrashctl(env []string, args ...string) cliResult {
	p.t.Helper()
	res := p.crashctl(env, args...)
	if res.code != 0 {
		p.t.Fatalf("crashctl %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), res.code, res.stdout, res.stderr)
	}
	return res
}

// grant runs the operator grant and requires the player's balance to rise
// by credits. A retry with the same grant key must leave it unchanged.
func (p *platform) grant(key, playerID string, credits int64) {
	p.t.Helper()
	before := p.grantedBalance(playerID)
	p.mustCrashctl(nil, "ledger", "grant", key, playerID, strconv.FormatInt(credits, 10))
	if got := p.grantedBalance(playerID); got != before+credits {
		p.t.Fatalf("%s: balance %d after granting %d, want %d", playerID, got, credits, before+credits)
	}
	p.mustCrashctl(nil, "ledger", "grant", key, playerID, strconv.FormatInt(credits, 10))
	if got := p.grantedBalance(playerID); got != before+credits {
		p.t.Fatalf("%s: balance %d after retrying grant %s, want %d: the grant key must make a retry a no-op", playerID, got, key, before+credits)
	}
}

func (p *platform) grantedBalance(playerID string) int64 {
	p.t.Helper()
	return p.obs.int(`SELECT coalesce(sum(credits_posted - debits_posted), 0) FROM orvex.ledger_accounts WHERE kind = 'player' AND player_id = $1`, playerID)
}

type engineConfig struct {
	createRounds  bool
	bettingWindow time.Duration
	// addr is the listen address; empty means 127.0.0.1:0.
	addr string
}

func (p *platform) startEngine(cfg engineConfig) *service {
	p.t.Helper()
	addr := cfg.addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	s := startService(p.t, "engine", p.bin.engine, []string{
		"ORVEX_DATABASE_URL=" + p.engineDSN,
		"ORVEX_SECRET_KEYRING_FILE=" + p.keyringFile,
		"ORVEX_ENGINE_ADDR=" + addr,
		"ORVEX_GATEWAY_TOKEN_FILE=" + p.tokenFile,
		"ORVEX_BETTING_WINDOW=" + cfg.bettingWindow.String(),
		"ORVEX_CREATE_ROUNDS=" + strconv.FormatBool(cfg.createRounds),
	})
	p.services = append(p.services, s)
	return s
}

// startGateway starts the gateway with no database URL and no keyring: it
// reaches state only through the engine.
func (p *platform) startGateway(engineAddr string) *service {
	p.t.Helper()
	s := startService(p.t, "gateway", p.bin.gateway, []string{
		"ORVEX_ENGINE_ADDR=" + engineAddr,
		"ORVEX_GATEWAY_ADDR=127.0.0.1:0",
		"ORVEX_GATEWAY_TOKEN_FILE=" + p.tokenFile,
		"ORVEX_PLAYERS_FILE=" + p.playersFile,
	})
	p.services = append(p.services, s)
	return s
}

func (p *platform) player(name string, gateway *service) *player {
	p.t.Helper()
	tok, ok := p.tokens[name]
	if !ok {
		p.t.Fatalf("no token for player %s", name)
	}
	return newPlayer(name, tok, "http://"+gateway.addr)
}

// requireNoSecretsInOutput requires that no service log and no crashctl
// output carries a password, key, token, or any extra value given.
func (p *platform) requireNoSecretsInOutput(extra ...string) {
	p.t.Helper()
	outputs := append([]string{}, p.cli...)
	names := make([]string, 0, len(p.cli)+len(p.services))
	for range p.cli {
		names = append(names, "crashctl")
	}
	for _, s := range p.services {
		outputs = append(outputs, s.log.String())
		names = append(names, s.name)
	}
	for i, out := range outputs {
		for _, secret := range append(append([]string{}, p.secrets...), extra...) {
			if strings.Contains(out, secret) {
				p.t.Errorf("%s output carries a secret value (%d bytes)", names[i], len(secret))
			}
		}
	}
}

// v2 export grants: the orvex_auditor set of docs/architecture/persistence.md
// section 14 plus SELECT on the ledger tables the v2 export reads (section
// 15). Nothing on round_secrets, so only a revealed secret is readable.
var auditorGrants = []string{
	`GRANT CONNECT ON DATABASE %[2]s TO %[1]s`,
	`GRANT USAGE ON SCHEMA orvex TO %[1]s`,
	`GRANT SELECT ON orvex.rounds, orvex.chain_records TO %[1]s`,
	`GRANT SELECT ON orvex.round_secret_reveals TO %[1]s`,
	`GRANT SELECT (round_id, sequence, state_hash) ON orvex.round_snapshots TO %[1]s`,
	`GRANT SELECT ON orvex.ledger_accounts, orvex.ledger_transactions, orvex.ledger_postings, orvex.ledger_holds, orvex.ledger_hold_releases TO %[1]s`,
}

var exportRecords = regexp.MustCompile(`(?m)^records (\d+) of (\d+) head ([0-9a-f]{64})$`)

// auditV2 exports the settled round as orvex/audit-history/v2 with an
// auditor role and no keyring, verifies the file with -expect-head set to
// the head the export printed, and requires that head to be the round's
// newest chain record. It returns the verifier report.
func (p *platform) auditV2(round [16]byte, records []storedRecord) string {
	p.t.Helper()
	password := randomHex(p.t)
	p.secrets = append(p.secrets, password)
	auditorDSN := p.loginRole("orvex_acceptance_auditor", password, "", auditorGrants...)
	id := string(canonjson.UUID(round))
	file := filepath.Join(p.t.TempDir(), id+".jsonl")

	exp := p.mustCrashctl([]string{"ORVEX_DATABASE_URL=" + auditorDSN},
		"audit", "export", "-format", verification.FormatV2, id, file)
	m := exportRecords.FindStringSubmatch(exp.stdout)
	if m == nil || m[1] != m[2] || !strings.Contains(exp.stdout, "\nsettled yes\n") {
		p.t.Fatalf("export report:\n%s\nwant every record exported and settled yes", exp.stdout)
	}
	head := m[3]
	last := records[len(records)-1]
	if n, _ := strconv.Atoi(m[1]); n != len(records) || head != last.Hash.String() {
		p.t.Fatalf("export: %s records head %s; database: %d records head %s", m[1], head, len(records), last.Hash)
	}

	f, err := os.Open(file) //nolint:gosec // G304: file is the export this test wrote
	if err != nil {
		p.t.Fatal(err)
	}
	firstLine, err := bufio.NewReader(f).ReadBytes('\n')
	_ = f.Close()
	if err != nil {
		p.t.Fatal(err)
	}
	var header struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(firstLine, &header); err != nil || header.Format != verification.FormatV2 {
		p.t.Fatalf("export header %s, want format %s", firstLine, verification.FormatV2)
	}

	ver := p.mustCrashctl([]string{}, "audit", "verify", "-expect-head", head, file)
	for _, want := range []string{
		"format " + verification.FormatV2 + " chain " + chain.Version + "\n",
		fmt.Sprintf("chain ok records %d head %s\n", len(records), head),
		"\nledger ok ",
	} {
		if !strings.Contains(ver.stdout, want) {
			p.t.Errorf("verify report:\n%s\nwant %q", ver.stdout, want)
		}
	}
	if !strings.HasSuffix(ver.stdout, "\nok\n") {
		p.t.Errorf("verify report:\n%s\nwant it to end with ok", ver.stdout)
	}
	return ver.stdout
}
