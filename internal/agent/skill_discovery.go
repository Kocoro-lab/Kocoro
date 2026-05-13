package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/skills"
)

// policySkillMarkers are substrings that, when found in a skill's description
// or frontmatter metadata, mark it as a platform-management / policy skill.
// A skill like `kocoro` whose description covers agents, schedules, skills,
// permissions and daemon config will match several of these.
var policySkillMarkers = []string{
	"platform",
	"configur", // configure / configuration
	"manage",
	"agent",
	"schedule",
	"skill",
	"permission",
	"daemon",
}

// Platform-intent scoring
//
// Pre-filter uses a weighted score instead of a phrase list to balance
// recall and precision. Rationale: missing a platform-management intent
// (FN) is expensive — the model drifts into raw bash/file_write on
// ~/.shannon/… and bypasses API validation — whereas a false positive
// (FP) costs only a few tokens of hint. The scorer is therefore generous
// but hardened with a calendar veto for the most common FP source.
//
// Tiers:
//   anchors       — unique Kocoro/infra terms, fire on sight, immune to veto
//   strong nouns  — platform indicators that can also appear elsewhere
//                   (e.g. "insurance agent"); EN matched by word boundary
//   verbs         — generic action words; only scored when paired with a
//                   strong noun or anchor (prevents "configure printer")
//   veto          — calendar/social language (meeting, standup, 会议, 予約)
//                   fully applied when no anchor; softened to half-weight
//                   when a strong noun+verb pair is present, so "create an
//                   agent for daily standup" still fires on the agent intent

const (
	anchorWeight     = 100
	strongNounWeight = 60
	verbWeight       = 25
	vetoWeight       = -60
	vetoSoftWeight   = -30
	scoreThreshold   = 50
)

// platformAnchors: substring-matched; any hit is near-certain platform intent.
var platformAnchors = []string{
	// EN / project-brand / tech tokens (share with ZH/JA — these are used
	// verbatim in all three languages)
	"kocoro",
	"shannon",
	"daemon",
	"heartbeat",
	"launchd",
	"schedule_create",
	"~/.shannon",
	"config.yaml",
	"allowed-tools",
	"mcp server",
	"mcp_server",
	// ZH platform anchors
	"定时任务",
	"守护进程",
	"计划任务",
	// JA platform anchors
	"定期実行",   // "scheduled execution" — tech term, near-certain platform
	"デーモン",   // katakana "daemon" — script-flexible spelling
	"スケジュール実行",
}

// platformStrongNouns: EN tokens matched by word boundary so "agenda" does
// not match "agent"; "insurance agent" still will but that's a rare FP.
var platformStrongNouns = []string{
	"agent",
	"schedule",
	"skill",
	"permission",
	"cron",
	"hook",
}

// platformStrongNounsZH: pure-CJK platform nouns. Substring-matched (CJK
// has no word boundaries). Pure-ASCII like "agent" is NOT listed here —
// the EN tokenizer extracts ASCII tokens cleanly from mixed-script text
// (e.g. "设置一个agent" → token "agent") so it's matched via the EN list.
var platformStrongNounsZH = []string{
	"权限",
	"技能",
	"定时",
}

// platformStrongNounsJA: Japanese platform nouns (katakana + kanji).
// Japanese developers commonly mix with English, so "agent" is already
// covered by the EN list; these catch pure-JA phrasings.
var platformStrongNounsJA = []string{
	"エージェント", // agent
	"スケジュール", // schedule
	"スキル",    // skill
	"パーミッション", // permission
	"フック",    // hook
	"定期",     // "regular/periodic" — strong signal with a verb
}

// platformVerbs: scored only when co-occurring with a strong noun or anchor.
// Multi-word entries ("set up") fall back to substring match.
var platformVerbs = []string{
	"create",
	"setup",
	"set up",
	"configure",
	"add",
	"manage",
	"enable",
	"disable",
	"remove",
}

// platformVerbsZH: substring-matched, scored only with noun/anchor present.
var platformVerbsZH = []string{
	"创建",
	"新建",
	"设置",
	"配置",
	"添加",
	"管理",
	"启用",
	"禁用",
	"删除",
}

// platformVerbsJA: Japanese action verbs. Scored only with noun/anchor.
// Note: 管理 shares kanji with ZH list but is matched once per message
// (stringSliceContains dedupe in scorer).
var platformVerbsJA = []string{
	"作成",  // create
	"新規作成", // new-create (common UI phrasing)
	"設定",  // set up / configure
	"追加",  // add
	"管理",  // manage
	"有効化", // enable
	"無効化", // disable
	"削除",  // delete
	"登録",  // register
}

// calendarVeto: strong negative signal suggesting calendar/meeting intent.
var calendarVeto = []string{
	"meeting",
	"appointment",
	"calendar",
	"reminder",
	"standup",
	"stand-up",
	"gym",
	"workout",
	"dinner",
	"doctor",
	"dentist",
	"date with",
}

// calendarVetoZH: Chinese calendar/social terms.
var calendarVetoZH = []string{
	"会议",
	"约会",
	"日历",
	"提醒事项",
	"站会",
	"早会",
	"医生",
	"牙医",
	"健身",
	"聚餐",
	"生日",
	"约饭",
}

// calendarVetoJA: Japanese calendar/social terms. 予定 intentionally left
// out because it overlaps with technical "scheduled task" usage.
var calendarVetoJA = []string{
	"予約",    // booking / reservation
	"ミーティング", // meeting
	"カレンダー",  // calendar
	"会議",    // meeting (shared kanji w/ ZH)
	"打ち合わせ", // discussion / meeting
	"アポ",    // appointment (informal)
	"アポイント",  // appointment
	"飲み会",   // drinking party
	"診察",    // medical consultation
	"通院",    // hospital visit
	"ジム",    // gym
	"デート",   // date
	"誕生日",   // birthday
	"忘年会",   // year-end party
	"歓迎会",   // welcome party
}

// isObserveOnlyMode reports whether SHANNON_SKILL_DISCOVERY_OBSERVE is set
// to "1" at call time. Read per-call rather than captured at package init
// so tests can flip it via t.Setenv without touching package state (no
// global mutation, no data race under -parallel). The env lookup cost is
// ~50ns — negligible vs. the small-model call this gates.
func isObserveOnlyMode() bool {
	return os.Getenv("SHANNON_SKILL_DISCOVERY_OBSERVE") == "1"
}

const discoveryPrompt = `Match the user message to available skills. Output skill names ONLY — no explanations, no commentary, no questions. The user may write in any language; skill descriptions are in English. Match by INTENT.

Rules:
- One skill name per line, nothing else
- "none" if no skill matches
- Be liberal: if the task COULD benefit from a skill, include it

Available skills:
%s

User message:
%s

Respond with skill names only:`

const discoveryTimeout = 5 * time.Second

var skillDebug = os.Getenv("SHANNON_SKILL_DEBUG") == "1"

// isPolicySkill reports whether the given skill looks like a platform /
// policy skill (one that should be preferred for agent/schedule/skill/daemon
// management intents). Matching is liberal — any policy marker appearing in
// the description or metadata text is enough.
func isPolicySkill(s *skills.Skill) bool {
	if s == nil {
		return false
	}
	hay := normalizeForMatch(s.Description)
	if hay != "" {
		for _, m := range policySkillMarkers {
			if strings.Contains(hay, m) {
				return true
			}
		}
	}
	// Also scan the metadata map (string leaves only) so skills with short
	// descriptions but rich frontmatter still qualify.
	metaHay := normalizeForMatch(flattenMetadataStrings(s.Metadata))
	if metaHay != "" {
		for _, m := range policySkillMarkers {
			if strings.Contains(metaHay, m) {
				return true
			}
		}
	}
	return false
}

// flattenMetadataStrings returns all string leaves in a nested metadata map
// joined by spaces. Non-string values are ignored.
func flattenMetadataStrings(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	var sb strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			sb.WriteString(t)
			sb.WriteByte(' ')
		case map[string]any:
			for _, vv := range t {
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	for _, v := range m {
		walk(v)
	}
	return sb.String()
}

// normalizeForMatch lower-cases the input using a unicode-aware fold so
// ASCII and non-ASCII characters compare consistently. CJK characters are
// unaffected by ToLower but calling it is still safe.
func normalizeForMatch(s string) string {
	if s == "" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		return unicode.ToLower(r)
	}, s)
}

// intentScoreBreakdown records what terms contributed to a platform-intent
// score. Emitted to stderr when SHANNON_SKILL_DEBUG=1 so thresholds and
// dictionaries can be tuned against real traffic.
type intentScoreBreakdown struct {
	Total   int
	Anchors []string
	Nouns   []string
	Verbs   []string
	Vetoes  []string
}

// tokenizeEN returns a set of ASCII-letter / digit / underscore tokens.
// CJK characters break the token boundary, which is exactly what we want:
//   "agented"               -> {"agented"}                (agent ≠ agented)
//   "设置一个agent"           -> {"agent"}                  (CJK breaks → clean token)
//   "write a python script" -> {"write", "a", "python", "script"}
// This gives us word-boundary semantics for ASCII terms AND lets EN tokens
// inside CJK text still match via the EN list — so the ZH noun/verb lists
// only need to carry pure-CJK terms.
func tokenizeEN(s string) map[string]bool {
	tokens := make(map[string]bool)
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens[cur.String()] = true
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return tokens
}

// isASCIIWord reports whether s is pure ASCII (no non-ASCII runes).
// Non-ASCII terms fall back to substring matching.
func isASCIIWord(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// containsTerm matches single-word ASCII terms against the token set and
// multi-word / non-ASCII terms against the raw lowercased string.
func containsTerm(text string, tokens map[string]bool, term string) bool {
	if term == "" {
		return false
	}
	if strings.Contains(term, " ") || !isASCIIWord(term) {
		return strings.Contains(text, term)
	}
	return tokens[term]
}

// scorePlatformIntent computes a platform-management intent score for the
// user's message and returns the breakdown. A score >= scoreThreshold
// indicates the pre-filter should fire. Weights and thresholds are const
// so they can be tuned without touching call sites.
func scorePlatformIntent(userText string) (int, intentScoreBreakdown) {
	bd := intentScoreBreakdown{}
	if userText == "" {
		return 0, bd
	}
	norm := normalizeForMatch(userText)
	enTokens := tokenizeEN(norm)

	// Anchors — substring match, platform-unique terms.
	for _, a := range platformAnchors {
		if strings.Contains(norm, a) {
			bd.Anchors = append(bd.Anchors, a)
		}
	}

	// Strong nouns — EN word-bounded, ZH/JA substring.
	for _, n := range platformStrongNouns {
		if containsTerm(norm, enTokens, n) {
			bd.Nouns = append(bd.Nouns, n)
		}
	}
	for _, n := range platformStrongNounsZH {
		if strings.Contains(norm, n) {
			// Avoid double-counting when the same token appears in both lists
			// (e.g. "agent" is both EN and ZH-list member).
			if !stringSliceContains(bd.Nouns, n) {
				bd.Nouns = append(bd.Nouns, n)
			}
		}
	}
	for _, n := range platformStrongNounsJA {
		if strings.Contains(norm, n) {
			if !stringSliceContains(bd.Nouns, n) {
				bd.Nouns = append(bd.Nouns, n)
			}
		}
	}

	hasAnchor := len(bd.Anchors) > 0
	hasNoun := len(bd.Nouns) > 0

	// Verbs — only scored when a noun or anchor provides context.
	if hasAnchor || hasNoun {
		for _, v := range platformVerbs {
			if containsTerm(norm, enTokens, v) {
				bd.Verbs = append(bd.Verbs, v)
			}
		}
		for _, v := range platformVerbsZH {
			if strings.Contains(norm, v) {
				if !stringSliceContains(bd.Verbs, v) {
					bd.Verbs = append(bd.Verbs, v)
				}
			}
		}
		for _, v := range platformVerbsJA {
			if strings.Contains(norm, v) {
				if !stringSliceContains(bd.Verbs, v) {
					bd.Verbs = append(bd.Verbs, v)
				}
			}
		}
	}

	// Vetoes.
	for _, v := range calendarVeto {
		if containsTerm(norm, enTokens, v) {
			bd.Vetoes = append(bd.Vetoes, v)
		}
	}
	for _, v := range calendarVetoZH {
		if strings.Contains(norm, v) {
			if !stringSliceContains(bd.Vetoes, v) {
				bd.Vetoes = append(bd.Vetoes, v)
			}
		}
	}
	for _, v := range calendarVetoJA {
		if strings.Contains(norm, v) {
			if !stringSliceContains(bd.Vetoes, v) {
				bd.Vetoes = append(bd.Vetoes, v)
			}
		}
	}

	// Compose score. Veto applies at most ONCE per message regardless of how
	// many veto terms matched — a message with "standup" + "reminder" should
	// not cumulatively out-weight a strong platform signal. Multi-term veto
	// is still evidence of calendar intent when no platform pair exists.
	score := len(bd.Anchors)*anchorWeight + len(bd.Nouns)*strongNounWeight + len(bd.Verbs)*verbWeight
	if len(bd.Vetoes) > 0 && !hasAnchor {
		hasStrongPair := hasNoun && len(bd.Verbs) > 0
		w := vetoWeight
		if hasStrongPair {
			w = vetoSoftWeight
		}
		score += w
	}

	bd.Total = score
	return score, bd
}

func stringSliceContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// matchPlatformIntent reports whether the user's message scores at or above
// the platform-intent threshold. Returns a compact marker (first anchor or
// noun, or "score=N") plus the fired bool. Signature kept (string, bool)
// for compatibility with existing call sites and tests.
func matchPlatformIntent(userText string) (string, bool) {
	score, bd := scorePlatformIntent(userText)
	fired := score >= scoreThreshold
	if skillDebug && (fired || score != 0) {
		fmt.Fprintf(os.Stderr, "[skill-discovery] intent score=%d fired=%v anchors=%v nouns=%v verbs=%v vetoes=%v\n",
			bd.Total, fired, bd.Anchors, bd.Nouns, bd.Verbs, bd.Vetoes)
	}
	if !fired {
		return "", false
	}
	switch {
	case len(bd.Anchors) > 0:
		return "anchor:" + bd.Anchors[0], true
	case len(bd.Nouns) > 0:
		return "noun:" + bd.Nouns[0], true
	default:
		return fmt.Sprintf("score=%d", bd.Total), true
	}
}

// policySkillPreFilter returns loaded skills that look like policy skills
// when the user's message contains an obvious platform-management phrase.
// Returns (nil, false) if either (a) no phrase matches or (b) no loaded
// skill qualifies as a policy skill. This is a short-circuit used BEFORE
// the small-model call so obvious cases always produce a hint even if the
// LLM times out or returns nothing.
func policySkillPreFilter(userText string, loaded []*skills.Skill) ([]*skills.Skill, bool) {
	marker, ok := matchPlatformIntent(userText)
	if !ok {
		return nil, false
	}
	if isObserveOnlyMode() {
		if skillDebug {
			fmt.Fprintf(os.Stderr, "[skill-discovery] observe-only: suppressing pre-filter fire (marker=%s)\n", marker)
		}
		return nil, false
	}
	var matched []*skills.Skill
	for _, s := range loaded {
		if isPolicySkill(s) {
			matched = append(matched, s)
		}
	}
	if len(matched) == 0 {
		if skillDebug {
			fmt.Fprintf(os.Stderr, "[skill-discovery] pre-filter marker %q matched but no policy skill loaded\n", marker)
		}
		return nil, false
	}
	if skillDebug {
		names := make([]string, len(matched))
		for i, s := range matched {
			names[i] = s.Name
		}
		fmt.Fprintf(os.Stderr, "[skill-discovery] pre-filter marker %q → %s\n", marker, strings.Join(names, ", "))
	}
	return matched, true
}

// mergeSkillMatches appends b's skills that are not already in a, preserving
// order and de-duplicating by Name.
func mergeSkillMatches(a, b []*skills.Skill) []*skills.Skill {
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	for _, s := range a {
		seen[s.Name] = true
	}
	out := a
	for _, s := range b {
		if !seen[s.Name] {
			out = append(out, s)
			seen[s.Name] = true
		}
	}
	return out
}

// discoverRelevantSkills calls a small-tier model to identify which skills
// are relevant to the user's message. Returns matched skills and usage.
// On timeout or error, returns nil (caller should proceed without discovery).
//
// A phrase-based pre-filter runs BEFORE the small-model call. When the user
// message contains an obvious platform-management phrase (EN or ZH) AND at
// least one loaded skill qualifies as a policy skill, those skills are
// guaranteed to be in the result — even if the small-model call times out,
// errors, or returns "none".
func discoverRelevantSkills(ctx context.Context, c ctxwin.Completer, userText string, loaded []*skills.Skill) ([]*skills.Skill, client.Usage) {
	if len(loaded) == 0 || userText == "" {
		return nil, client.Usage{}
	}

	preFiltered, _ := policySkillPreFilter(userText, loaded)

	var catalog strings.Builder
	for _, s := range loaded {
		fmt.Fprintf(&catalog, "- %s: %s\n", s.Name, s.Description)
	}

	prompt := fmt.Sprintf(discoveryPrompt, catalog.String(), userText)

	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	resp, err := c.Complete(ctx, client.CompletionRequest{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent(prompt)},
		},
		ModelTier:   "small",
		Temperature: 0.0,
		MaxTokens:   30,
		CacheSource: "helper",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[skill-discovery] error: %v\n", err)
		// Small-model failed, but pre-filter hits still produce a hint.
		return preFiltered, client.Usage{}
	}

	matched := parseDiscoveryOutput(resp.OutputText, loaded)
	// Pre-filter takes priority so policy skills appear first in the hint.
	matched = mergeSkillMatches(preFiltered, matched)
	if skillDebug {
		if len(matched) > 0 {
			names := make([]string, len(matched))
			for i, s := range matched {
				names[i] = s.Name
			}
			fmt.Fprintf(os.Stderr, "[skill-discovery] matched: %s\n", strings.Join(names, ", "))
		} else {
			fmt.Fprintf(os.Stderr, "[skill-discovery] no match (raw: %q)\n", resp.OutputText)
		}
	}
	return matched, resp.Usage
}

// parseDiscoveryOutput validates discovery model output against loaded skills.
func parseDiscoveryOutput(output string, loaded []*skills.Skill) []*skills.Skill {
	nameSet := make(map[string]*skills.Skill, len(loaded))
	for _, s := range loaded {
		nameSet[s.Name] = s
	}

	var matched []*skills.Skill
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.EqualFold(name, "none") {
			continue
		}
		if s, ok := nameSet[name]; ok && !seen[name] {
			matched = append(matched, s)
			seen[name] = true
		}
	}
	return matched
}

// formatDiscoveryHint builds a <system-reminder> for matched skills.
func formatDiscoveryHint(matched []*skills.Skill) string {
	if len(matched) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("<system-reminder>\nSkills relevant to your task:\n")
	for _, s := range matched {
		desc := s.Description
		runes := []rune(desc)
		if len(runes) > 120 {
			desc = string(runes[:117]) + "..."
		}
		fmt.Fprintf(&sb, "- %s: %s\n", s.Name, desc)
	}
	sb.WriteString("\nCall use_skill(\"<name>\") to load full instructions before proceeding.\n</system-reminder>")
	return sb.String()
}
