package agent

// Regression corpus for the mint/consume layer of isReadMCPName.
//
// Promoted from the candidate-rule experiment that measured the layer at
// zero false kills on 40 genuine read names and 19/19 catches on
// minting/consuming get_* names (the old rule caught 1/19, delaying the
// side-effect force-stop from call 4 to call 6). Both corpora below now pin
// the PRODUCTION function in both directions.

import "testing"

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

	production := evaluate(isReadMCPName)
	t.Logf("production: false_kills=%d caught=%d/%d missed=%d",
		production.falseKills, production.caught, len(mintingGetNames), production.missed)

	// Pin the promoted rule in both directions: any false kill on a genuine
	// read name or any missed minting name is a regression.
	if production.falseKills != 0 {
		t.Errorf("mint/consume layer false-killed %d genuine read names", production.falseKills)
	}
	if production.missed != 0 {
		t.Errorf("mint/consume layer missed %d minting/consuming names", production.missed)
	}
}
