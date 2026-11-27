//go:build integration && acceptance && spectatorload

package firstround

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/juano-morello/orvex/internal/audit/verification"
	"github.com/juano-morello/orvex/internal/canonjson"
	v1 "github.com/juano-morello/orvex/internal/game/crash/v1"
	"github.com/juano-morello/orvex/internal/ledger"
)

// burstVerification is retained with the trial report, including failures.
type burstVerification struct {
	RoundID       string           `json:"roundId"`
	PlayerResults map[int64]string `json:"playerResultsByAdmissionSequence"`
	Problems      []string         `json:"problems"`
	Attempts      int              `json:"attempts"`
	Admissions    int              `json:"admissions"`
	Records       int              `json:"records"`
	Accepted      int              `json:"accepted"`
	Rejected      int              `json:"rejected"`
	Retries       int              `json:"retries"`
	DeadlineUS    int64            `json:"deadline_us"`
	Head          string           `json:"head"`
	AuditOK       bool             `json:"audit_ok"`
}

// burstCommandID independently reproduces the public principal/client ID mapping.
func burstCommandID(player, client string) ([16]byte, error) {
	id, err := canonjson.ParseUUID(canonjson.String(client))
	if err != nil {
		return [16]byte{}, err
	}
	h := sha256.New()
	h.Write([]byte("orvex/command-id/v1\x00"))
	h.Write([]byte(player))
	h.Write([]byte{0})
	h.Write(id[:])
	var out [16]byte
	copy(out[:], h.Sum(nil))
	out[6] = out[6]&0x0f | 0x80
	out[8] = out[8]&0x3f | 0x80
	return out, nil
}

func reconcileBurst(t *testing.T, p *platform, roundID [16]byte, attempts []burstAttempt) burstVerification {
	t.Helper()
	v := burstVerification{
		RoundID: string(canonjson.UUID(roundID)), Attempts: len(attempts), PlayerResults: map[int64]string{},
	}
	problem := func(format string, args ...any) { v.Problems = append(v.Problems, fmt.Sprintf(format, args...)) }
	admissions := p.obs.admissions(roundID)
	records := p.obs.records(roundID)
	v.Admissions, v.Records = len(admissions), len(records)
	byID := make(map[[16]byte]admitted, len(admissions))
	bySeq := make(map[int64]storedRecord, len(records))
	for _, a := range admissions {
		byID[a.CommandID] = a
	}
	for _, r := range records {
		bySeq[r.Sequence] = r
		for _, e := range r.Events {
			if c, ok := e.(v1.RoundCrashed); ok {
				v.DeadlineUS = c.CrashDeadlineUnixMicro
			}
		}
	}
	for _, a := range admissions {
		r, ok := bySeq[a.Sequence]
		if !ok || r.Genesis != nil {
			problem("admission %d has no terminal record", a.Sequence)
			continue
		}
		if r.Record.CommandID != a.CommandID || r.Record.EffectiveTimeUnixMicro != a.EffectiveTime || r.Command != a.Command {
			problem("admission %d differs from terminal record", a.Sequence)
		}
		if r.Record.Result.Rejected {
			v.Rejected++
		} else {
			v.Accepted++
		}
		switch a.Command.(type) {
		case v1.PlaceBet, v1.CashOut:
			outcome := "accepted"
			if r.Record.Result.Rejected {
				outcome = r.Record.Result.Reason
			}
			v.PlayerResults[a.Sequence] = outcome
		}
		if _, ok := a.Command.(v1.CashOut); ok && v.DeadlineUS != 0 && a.EffectiveTime >= v.DeadlineUS {
			for _, e := range r.Events {
				if _, ok := e.(v1.BetCashedOut); ok {
					problem("cash-out %d at %d succeeded at or after deadline %d", a.Sequence, a.EffectiveTime, v.DeadlineUS)
				}
			}
		}
	}
	for _, r := range records {
		if r.Genesis == nil {
			if _, ok := byID[r.Record.CommandID]; !ok {
				problem("record %d has no admission", r.Sequence)
			}
		}
	}
	seen := map[[16]byte]bool{}
	for _, a := range attempts {
		id, err := burstCommandID(a.Player, a.ClientID)
		if err != nil {
			problem("player %q client ID %q: %v", a.Player, a.ClientID, err)
			continue
		}
		if seen[id] {
			v.Retries++
		}
		seen[id] = true
		stored, ok := byID[id]
		if !ok {
			if a.HTTPStatus != 409 || (a.Error != "round_closed" && a.Error != "round_sealed") {
				problem("command %s for %s has no admission; response status %d error %q", a.ClientID, a.Player, a.HTTPStatus, a.Error)
			}
			continue
		}
		if a.HTTPStatus == 202 && a.Error == "" && (a.Seq != stored.Sequence || a.EffectiveUS != stored.EffectiveTime) {
			problem("receipt for %s/%s differs from admission %d at %d", a.Player, a.ClientID, stored.Sequence, stored.EffectiveTime)
		}
		if a.HTTPStatus == 409 && (a.Error == "round_closed" || a.Error == "round_sealed") {
			problem("gateway reported %s for stored admission %d", a.Error, stored.Sequence)
		}
		switch cmd := stored.Command.(type) {
		case v1.PlaceBet:
			if string(cmd.Player) != a.Player || a.Kind != "bet" || cmd.BetCredits != benchBetCredits {
				problem("command %s/%s does not match place bet", a.Player, a.ClientID)
			}
		case v1.CashOut:
			if string(cmd.Player) != a.Player || (a.Kind != "cashout" && a.Kind != "retry") {
				problem("command %s/%s does not match cash-out", a.Player, a.ClientID)
			}
		default:
			problem("command %s/%s maps to lifecycle command", a.Player, a.ClientID)
		}
	}
	for _, a := range admissions {
		switch a.Command.(type) {
		case v1.PlaceBet, v1.CashOut:
			if !seen[a.CommandID] {
				problem("player admission %d has no recorded attempt", a.Sequence)
			}
		}
	}
	if len(records) == 0 {
		problem("round has no records")
		return v
	}
	v.Head = records[len(records)-1].Hash.String()
	settled := p.obs.settled(roundID)
	if !settled || p.obs.admissionState(roundID) != "sealed" {
		problem("round is not settled and sealed")
	}
	var unreleased int64
	err := p.obs.db.QueryRow(context.Background(), `SELECT count(*) FROM orvex.ledger_holds h
		LEFT JOIN orvex.ledger_hold_releases r USING (round_id, sequence, account_id)
		WHERE h.round_id = $1 AND r.round_id IS NULL`, roundID).Scan(&unreleased)
	if err != nil {
		problem("round holds: %v", err)
	} else if unreleased != 0 {
		problem("round has %d unreleased holds", unreleased)
	}
	var balance, held int64
	err = p.obs.db.QueryRow(context.Background(), `SELECT credits_posted - debits_posted, held FROM orvex.ledger_accounts WHERE account_id = $1`, ledger.RoundAccountID(roundID)).Scan(&balance, &held)
	if err != nil {
		problem("round ledger account: %v", err)
	} else if balance != 0 || held != 0 {
		problem("round ledger account balance %d held %d", balance, held)
	}
	if !settled {
		return v
	}
	id := string(canonjson.UUID(roundID))
	file := filepath.Join(t.TempDir(), "burst-"+id+".jsonl")
	exp := p.crashctl(nil, "audit", "export", "-format", verification.FormatV2, id, file)
	m := exportRecords.FindStringSubmatch(exp.stdout)
	if exp.code != 0 {
		problem("settled audit export failed (exit %d): %s", exp.code, exp.stderr)
		return v
	}
	if m == nil || m[1] != m[2] || m[1] != strconv.Itoa(len(records)) || m[3] != v.Head || !strings.Contains(exp.stdout, "\nsettled yes\n") {
		problem("settled audit export report differs from database: %s", exp.stdout)
		return v
	}
	ver := p.crashctl([]string{}, "audit", "verify", "-expect-head", v.Head, file)
	if ver.code != 0 || !strings.Contains(ver.stdout, "\nledger ok ") || !strings.HasSuffix(ver.stdout, "\nok\n") {
		problem("settled audit verify failed (exit %d): %s %s", ver.code, ver.stdout, ver.stderr)
		return v
	}
	v.AuditOK = true
	return v
}

// checkPlayerWorkload checks each funded player's receipts against the work
// scheduled for that player, independent of the order responses arrived.
func checkPlayerWorkload(players []string, arm string, attempts []burstAttempt, v burstVerification) []string {
	var problems []string
	type key struct{ player, kind string }
	byPlayer := make(map[key]burstAttempt, len(attempts))
	for _, a := range attempts {
		k := key{a.Player, a.Kind}
		if _, exists := byPlayer[k]; exists {
			problems = append(problems, fmt.Sprintf("duplicate %s attempt for %s", a.Kind, a.Player))
		}
		byPlayer[k] = a
	}
	kinds := []string{"bet", "cashout"}
	if arm == "retry" {
		kinds = append(kinds, "retry")
	}
	for _, player := range players {
		for _, kind := range kinds {
			k := key{player, kind}
			a, exists := byPlayer[k]
			delete(byPlayer, k)
			if !exists {
				problems = append(problems, fmt.Sprintf("missing %s attempt for %s", kind, player))
				continue
			}
			if kind == "cashout" && arm == "retry" {
				r := byPlayer[key{player, "retry"}]
				if a.ClientID != r.ClientID || a.Seq != r.Seq || a.EffectiveUS != r.EffectiveUS {
					problems = append(problems, fmt.Sprintf("retry identity or receipt differs for %s", player))
				}
			}
			if arm == "deadline" && kind == "cashout" {
				if a.HTTPStatus == 409 && (a.Error == "round_closed" || a.Error == "round_sealed") {
					continue
				}
				if a.EffectiveUS >= v.DeadlineUS && v.PlayerResults[a.Seq] == "deadline_passed" {
					continue
				}
			}
			if a.HTTPStatus != 202 || a.Error != "" || v.PlayerResults[a.Seq] != "accepted" {
				problems = append(problems, fmt.Sprintf("%s for %s did not produce an accepted outcome", kind, player))
			}
		}
	}
	if len(byPlayer) > 0 {
		problems = append(problems, "unexpected player or command kind in workload")
	}
	return problems
}
