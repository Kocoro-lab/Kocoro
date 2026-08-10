package agent

// Experiment: a stricter read-name heuristic for MCP tools.
//
// isReadMCPName currently classifies minting/consuming get_* names
// (get_token, get_next_job, get_upload_url…) as read-only, which widens the
// loop detector's read-only tolerance for tools that actually have side
// effects (force moves from call 4 to call 6; corpus case
// loop_misclassified_get_token_side_effect pins that behavior). This test
// implements a CANDIDATE rule — the production function is untouched — and
// scores both rules on two labeled corpora, so the trade-off is measured
// rather than argued. If the candidate is ever promoted, this file becomes
// the regression corpus for it.

import "testing"

// candidateIsReadMCPName = current rule + a mint/consume detection layer:
// a name that passes the verb check is still NOT read-only when it names a
// minted/consumed resource (token, lock, lease, nonce, otp, credential,
// secret, ticket) or a queue-consuming "next <work item>" / presigned-URL
// pattern. Deliberately narrow: ambiguous nouns (session, key, url alone)
// stay read-only to avoid false kills on common read tools.
func candidateIsReadMCPName(name string) bool {
	if !isReadMCPName(name) {
		return false
	}
	tokens := tokenizeMCPName(name)
	minted := map[string]bool{
		"token": true, "tokens": true, "lock": true, "locks": true,
		"lease": true, "leases": true, "nonce": true, "otp": true,
		"credential": true, "credentials": true, "secret": true, "secrets": true,
		"ticket": true, "tickets": true,
	}
	workItems := map[string]bool{"task": true, "job": true, "message": true, "item": true, "event": false}
	urlMinters := map[string]bool{"upload": true, "signed": true, "presigned": true}
	for i, tok := range tokens {
		if minted[tok] {
			return false
		}
		if tok == "next" && i+1 < len(tokens) && workItems[tokens[i+1]] {
			return false
		}
		if urlMinters[tok] && i+1 < len(tokens) && tokens[i+1] == "url" {
			return false
		}
	}
	return true
}

func tokenizeMCPName(name string) []string {
	var tokens []string
	current := ""
	for _, r := range name {
		if r == '_' || r == '-' {
			if current != "" {
				tokens = append(tokens, current)
				current = ""
			}
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		current += string(r)
	}
	if current != "" {
		tokens = append(tokens, current)
	}
	return tokens
}

// genuineReadNames: common MCP read tools that MUST stay read-only under any
// candidate — a single false kill here disqualifies the rule.
var genuineReadNames = []string{
	"list_pages", "get_page", "get_message", "search_messages", "read_file",
	"get_event", "list_events", "get_user", "list_users", "query_database",
	"get_issue", "list_issues", "search_issues", "get_pull_request",
	"list_commits", "get_file_contents", "read_page", "get_weather",
	"get_balance", "list_channels", "get_channel", "get_thread",
	"search_threads", "get_document", "list_documents", "get_calendar",
	"list_calendars", "get_task", "list_tasks", "get_status", "get_metrics",
	"fetch_page", "get_charge", "get_transition", "get_next_page",
	"notion_list_pages", "google_gmail_search_messages", "get_session_replay",
	"describe_table", "find_duplicates",
}

// mintingGetNames: names built on read verbs whose real operation mints,
// consumes, or acquires — the calls the current rule wrongly relaxes.
var mintingGetNames = []string{
	"get_token", "get_access_token", "get_refresh_token", "oauth_get_token",
	"get_lock", "acquire_get_lock", "get_lease", "get_nonce", "get_otp",
	"get_temp_credentials", "get_credentials", "get_client_secret",
	"get_ticket", "get_next_task", "get_next_job", "fetch_next_message",
	"get_upload_url", "get_signed_url", "get_presigned_url",
}

func TestReadNameHeuristicExperiment(t *testing.T) {
	type score struct{ falseKills, caught, missed int }
	evaluate := func(rule func(string) bool) score {
		var s score
		for _, name := range genuineReadNames {
			if !rule(name) {
				s.falseKills++
				t.Logf("  false kill: %s", name)
			}
		}
		for _, name := range mintingGetNames {
			if rule(name) {
				s.missed++
			} else {
				s.caught++
			}
		}
		return s
	}

	current := evaluate(isReadMCPName)
	t.Logf("current  : false_kills=%d caught=%d/%d missed=%d",
		current.falseKills, current.caught, len(mintingGetNames), current.missed)
	candidate := evaluate(candidateIsReadMCPName)
	t.Logf("candidate: false_kills=%d caught=%d/%d missed=%d",
		candidate.falseKills, candidate.caught, len(mintingGetNames), candidate.missed)

	// Pin the measured outcome so corpus or rule edits surface loudly.
	if current.falseKills != 0 {
		t.Errorf("current rule false-killed %d genuine read names", current.falseKills)
	}
	if candidate.falseKills != 0 {
		t.Errorf("candidate rule false-killed %d genuine read names — disqualified", candidate.falseKills)
	}
	if candidate.caught <= current.caught {
		t.Errorf("candidate caught %d minting names, no better than current %d", candidate.caught, current.caught)
	}
}
