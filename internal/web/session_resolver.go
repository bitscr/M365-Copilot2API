package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// sessionBinding 璁板綍涓€娆″唴瀹归敭澶嶇敤鐨勪細璇濄€侷dentity 瀛楁锛圛P/user锛変粎浣?
// 璇婃柇鍏冩暟鎹繚鐣欙紝鍖归厤鍒ゅ畾鍙緷璧栦笂涓嬫枃鍐呭锛岃 Resolve 鐨勫唴瀹归敭閫昏緫銆?
type sessionBinding struct {
	SessionID      string    `json:"sessionId"`
	ConversationID string    `json:"conversationId"`
	AccountID      string    `json:"accountId"`
	CreatedAt      time.Time `json:"createdAt"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
	IPFingerprint  string    `json:"ipFingerprint,omitempty"`
	UserField      string    `json:"userField,omitempty"`
	ContextFinger  string    `json:"contextFinger,omitempty"`
	// ContentHash is the canonical full-history fingerprint (role + content +
	// tool call name/args, IDs ignored, text CALL_TOOL normalized). Two
	// bindings with the same ContentHash and tenant are the SAME logical
	// conversation even when the upstream M365 handed out a new cloud
	// ConversationID — Bind migrates instead of forking a duplicate session.
	ContentHash string `json:"contentHash,omitempty"`
	// ContextHistory 鎸佷箙鍖栦繚瀛樻渶杩戜竴娆″崗璁殑瀹屾暣娑堟伅锛屼緵閲嶅惎鍚庣户缁仛
	// 鍐呭鍓嶇紑鍖归厤锛岄伩鍏嶈繘绋嬮噸鍚鑷存墍鏈変細璇濋敭鍏ㄩ儴澶辨晥銆?
	ContextHistory []oaiMsg `json:"contextHistory,omitempty"`
	// Tenant isolates a binding to the API key that created it. Every read,
	// match, resume, and delete is scoped to the caller's tenant so one key can
	// never touch another key's conversations. An empty tenant marks a legacy
	// binding (created before this field existed): it is treated as unowned and
	// is never returned to a keyed caller.
	Tenant string `json:"tenant,omitempty"`
	// ExplicitID is the client-supplied X-M365-Session-Id. It is namespaced per
	// tenant via byExplicit so two tenants may use the same id without colliding.
	ExplicitID string `json:"explicitId,omitempty"`
}

type sessionResolver struct {
	mu          sync.Mutex
	path        string
	sessions    map[string]sessionBinding
	byExplicit  map[string]string // explicitID -> sessionID
	byUserField map[string]string // userField -> sessionID
	byIPFinger  map[string]string // ipFingerprint -> sessionID
	byContext   map[string]string // contextFingerprint -> sessionID
	ttl         time.Duration
	contextTTL  time.Duration
	maxSessions int
	persist     *persistStore
}

const defaultMaxSessions = 1000

func openSessionResolver() *sessionResolver {
	// 闂茬疆 2 灏忔椂鍗宠涓鸿繃鏈燂紙鐢ㄦ埛锛? 灏忔椂涓嶆椿璺冨凡缁忕畻涔咃級銆備細璇濊繃鏈熷悗
	// 浠?sessions.json 鍓旈櫎锛屼簯绔璇濅氦缁?auto_cleanup 鎸夌浉鍚岀獥鍙ｅ洖鏀躲€?
	ttl := 2 * time.Hour
	if v := os.Getenv("M365_SESSION_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			ttl = d
		}
	}
	contextTTL := 2 * time.Hour
	if v := os.Getenv("M365_CONTEXT_TTL_MINUTES"); v != "" {
		if d, err := time.ParseDuration(v + "m"); err == nil {
			contextTTL = d
		}
	}
	path := os.Getenv("M365_SESSION_CACHE")
	if path == "" {
		path = "sessions.json"
	}
	sr := &sessionResolver{
		path:        path,
		sessions:    map[string]sessionBinding{},
		byExplicit:  map[string]string{},
		byUserField: map[string]string{},
		byIPFinger:  map[string]string{},
		byContext:   map[string]string{},
		ttl:         ttl,
		contextTTL:  contextTTL,
		maxSessions: defaultMaxSessions,
	}
	sr.persist = &persistStore{flush: sr.flush}
	sr.loadLocked()
	return sr
}

func (sr *sessionResolver) loadLocked() {
	b, err := os.ReadFile(sr.path)
	if err != nil {
		return
	}
	var list []sessionBinding
	if err := json.Unmarshal(b, &list); err != nil {
		// Never fail silently: a corrupt/foreign-format sessions.json silently
		// drops every binding, which looks like "sessions stopped resuming".
		log.Printf("[session-resolver] failed to unmarshal %s: %v", sr.path, err)
		return
	}
	now := time.Now().UTC()
	for _, s := range list {
		if now.Sub(s.LastUsedAt) > sr.ttl {
			continue
		}
		sr.reindexLocked(s)
	}
}

// flush 在锁内生成快照，锁外写盘。
func (sr *sessionResolver) flush() error {
	sr.mu.Lock()
	list := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		list = append(list, s)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	sr.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(sr.path, b, 0o600)
}

func (sr *sessionResolver) reindexLocked(s sessionBinding) {
	sr.sessions[s.SessionID] = s
	if s.ExplicitID != "" {
		sr.byExplicit[explicitKey(s.Tenant, s.ExplicitID)] = s.SessionID
	}
	if s.UserField != "" {
		sr.byUserField[s.UserField] = s.SessionID
	}
	if s.IPFingerprint != "" {
		sr.byIPFinger[s.IPFingerprint] = s.SessionID
	}
	if s.ContextFinger != "" {
		sr.byContext[s.ContextFinger] = s.SessionID
	}
}

func (sr *sessionResolver) evictLocked() {
	now := time.Now().UTC()
	for id, s := range sr.sessions {
		if now.Sub(s.LastUsedAt) > sr.ttl {
			sr.dropLocked(id, s)
		}
	}
	if len(sr.sessions) > sr.maxSessions {
		// Bound memory by dropping the least recently used sessions.
		ids := make([]string, 0, len(sr.sessions))
		last := make(map[string]time.Time, len(sr.sessions))
		for id, s := range sr.sessions {
			ids = append(ids, id)
			last[id] = s.LastUsedAt
		}
		sort.Slice(ids, func(i, j int) bool { return last[ids[i]].Before(last[ids[j]]) })
		for _, id := range ids[:len(sr.sessions)-sr.maxSessions] {
			sr.dropLocked(id, sr.sessions[id])
		}
	}
}

func (sr *sessionResolver) dropLocked(id string, s sessionBinding) {
	delete(sr.sessions, id)
	if s.ExplicitID != "" {
		delete(sr.byExplicit, explicitKey(s.Tenant, s.ExplicitID))
	}
	if sr.byUserField[s.UserField] == id {
		delete(sr.byUserField, s.UserField)
	}
	if sr.byIPFinger[s.IPFingerprint] == id {
		delete(sr.byIPFinger, s.IPFingerprint)
	}
	if sr.byContext[s.ContextFinger] == id {
		delete(sr.byContext, s.ContextFinger)
	}
}

type ResolveResult struct {
	SessionID      string
	ConversationID string
	AccountID      string
	MatchedBy      string
	IsNew          bool
	// HistoryLen 鏄鐢ㄥ懡涓椂"浜戠瀵硅瘽宸插寘鍚殑娑堟伅鏉℃暟"锛?
	// 鍗冲閲忓彂閫佺殑璧风偣涓嬫爣锛坆ody.Messages[HistoryLen:] 鍙彂鏂板閮ㄥ垎锛夈€?
	HistoryLen int
}

// clientIPFingerprint derives the resolver's client identity. What it
// includes is configurable via M365_FINGERPRINT_MODE so deployments are not
// tied to a stable client IP (mobile egress, WARP, NAT, or rotating proxies
// would otherwise break session continuity):
//
//	ip_ua (default) — client IP + User-Agent; most precise, requires stable egress IP.
//	ua              — User-Agent only; survives IP rotation, still separates agents.
//	off             — no client identity; tenant + message content only (permissive).
//
// The session resolver additionally scopes by tenant and requires content
// fingerprints to match, so lowering the identity strength never re-enables
// cross-tenant bleed.
func clientIPFingerprint(r *http.Request) string {
	ua := r.Header.Get("User-Agent")
	switch fingerprintMode() {
	case "ua":
		h := sha256.Sum256([]byte(ua))
		return hex.EncodeToString(h[:16])
	case "off", "none":
		return ""
	}
	// default: ip_ua
	ip := clientIP(r)
	data := ip + "|" + ua
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

var (
	fingerprintModeOnce sync.Once
	fingerprintModeVal  string
)

func fingerprintMode() string {
	fingerprintModeOnce.Do(func() {
		fingerprintModeVal = strings.ToLower(strings.TrimSpace(os.Getenv("M365_FINGERPRINT_MODE")))
	})
	return fingerprintModeVal
}

func contextFingerprint(messages []oaiMsg) string {
	if len(messages) == 0 {
		return ""
	}
	var parts []string
	limit := len(messages)
	if limit > 3 {
		limit = 3
	}
	for i := len(messages) - limit; i < len(messages); i++ {
		m := messages[i]
		parts = append(parts, m.Role+":"+contentToString(m.Content))
	}
	data := strings.Join(parts, "||")
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:16])
}

func (sr *sessionResolver) Resolve(r *http.Request, body *oaiReq) ResolveResult {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	tenant := tenantFromRequest(r)
	explicitID := r.Header.Get("X-M365-Session-Id")

	// 瀹㈡埛绔樉寮忔寚瀹氱殑浼氳瘽 ID 鏄渶楂樹紭鍏堢殑缁帴璇箟锛氫笉鍙備笌浠讳綍韬唤鍒ゅ畾锛?
	// 鐢辫皟鐢ㄦ柟涓诲姩鍐冲畾瑕佺户缁摢涓簯绔璇濄€?
	if explicitID != "" {
		if sessID, ok := sr.byExplicit[explicitKey(tenant, explicitID)]; ok {
			if sess, ok := sr.sessions[sessID]; ok && sess.Tenant == tenant {
				sess.LastUsedAt = time.Now().UTC()
				sr.sessions[sessID] = sess
				sr.persist.markDirty()
				return ResolveResult{
					SessionID:      sess.SessionID,
					ConversationID: sess.ConversationID,
					AccountID:      sess.AccountID,
					MatchedBy:      "explicit",
					IsNew:          false,
					HistoryLen:     len(sess.ContextHistory),
				}
			}
		}
	}

	// 鍐呭閿細鍗忚娑堟伅鍚嶅簭鍒椾弗鏍肩瓑浜庢煇涓凡璁板綍浼氳瘽鐨勫巻鍙叉椂鐩存帴澶嶇敤杩欎釜
	// 浜戠瀵硅瘽锛屼絾鍙湪鍚屼竴 IP/UA 鎸囩汗涓嬶紝閬垮厤鐭秷鎭湪涓嶅悓鐢ㄦ埛闂翠簰绔?
	// HistoryLen 杩斿洖璇ュ墠缂€闀垮害锛屼笂灞傛嵁姝ゅ彧鍙戦€?messages[HistoryLen:] 澧為噺銆?
	ipFinger := clientIPFingerprint(r)
	if bestID, n := sr.matchContextLocked(tenant, ipFinger, body.Messages); bestID != "" {
		sess := sr.sessions[bestID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[bestID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_prefix_%d", n),
			IsNew:          false,
			HistoryLen:     n,
		}
	}

	// 寮辩害鏉熷厹搴曪細鍐呭涓嶆瀯鎴愪弗鏍煎墠缂€锛屼絾涓庢煇涓巻鍙查珮搴︾浉浼硷紙濡傚鎴风
	// 鏈湴鎴柇浜嗗巻鍙诧級锛屼粛澶嶇敤璇ヤ細璇濄€傛鏃跺閲忚竟鐣屾湭鐭ワ紝涓婂眰鍙戦€佸叏閲忋€?
	suffixID, suffixN := sr.matchSuffixLocked(tenant, ipFinger, body.Messages)
	if suffixID != "" {
		sess := sr.sessions[suffixID]
		sess.LastUsedAt = time.Now().UTC()
		sr.sessions[suffixID] = sess
		sr.persist.markDirty()
		return ResolveResult{
			SessionID:      sess.SessionID,
			ConversationID: sess.ConversationID,
			AccountID:      sess.AccountID,
			MatchedBy:      fmt.Sprintf("context_suffix_%d", suffixN),
			IsNew:          false,
			HistoryLen:     suffixN,
		}
	}

	return ResolveResult{IsNew: true}
}

func (sr *sessionResolver) matchSuffixLocked(tenant, ipFinger string, messages []oaiMsg) (string, int) {
	if len(messages) < 2 {
		return "", 0
	}
	type match struct {
		id     string
		n      int
		recent time.Time
	}
	best := match{}
	minSuffix := 2
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.Tenant != tenant {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		hist := sess.ContextHistory
		if len(hist) < minSuffix {
			continue
		}
		n := suffixMatchLen(hist, messages)
		if overlap := rollingOverlapLen(hist, messages); overlap > n {
			n = overlap
		}
		// A pure tool round tail — [assistant(tool_calls), tool(result)] —
		// is generic: concurrent tasks on the same box often produce
		// identical tool invocations and outputs (e.g. git status on the
		// same repo). Binding on such a tail makes task B resume task A's
		// cloud conversation and inherit its history, which surfaces as the
		// local agent trying to execute files/names that never existed in
		// its own task. Only bind when the matched window contains a real
		// conversational turn: a user message plus an assistant or tool
		// message.
		if n >= minSuffix && suffixIsConversational(hist, n) && (n > best.n || (n == best.n && sess.LastUsedAt.After(best.recent))) {
			best = match{id: id, n: n, recent: sess.LastUsedAt}
		}
	}
	return best.id, best.n
}

// suffixIsConversational reports whether the matched tail window of a stored
// history forms a conversational turn group (at least one user message and at
// least one assistant or tool message). Pure tool-result tails never qualify,
// so unrelated tasks cannot bind to each other through identical tool output.
func suffixIsConversational(hist []oaiMsg, n int) bool {
	user := false
	other := false
	for i := len(hist) - n; i < len(hist); i++ {
		switch hist[i].Role {
		case "user":
			user = true
		case "assistant", "tool", "function":
			other = true
		}
	}
	return user && other
}

func suffixMatchLen(hist, msgs []oaiMsg) int {
	maxN := len(hist)
	if maxN > len(msgs) {
		maxN = len(msgs)
	}
	n := 0
	for i := 1; i <= maxN; i++ {
		if messagesEqual(hist[len(hist)-i], msgs[len(msgs)-i]) {
			n = i
		} else {
			break
		}
	}
	return n
}

// rollingOverlapLen returns the longest suffix of stored history that matches
// a prefix of the incoming rolling-window request. Messages after that prefix
// are the new turn and should be sent incrementally on the existing cloud
// conversation.
func rollingOverlapLen(hist, msgs []oaiMsg) int {
	maxN := len(hist)
	if maxN > len(msgs) {
		maxN = len(msgs)
	}
	for n := maxN; n >= 1; n-- {
		matched := true
		for i := 0; i < n; i++ {
			if !messagesEqual(hist[len(hist)-n+i], msgs[i]) {
				matched = false
				break
			}
		}
		if matched {
			return n
		}
	}
	return 0
}

// matchContextLocked 浠庡叏閮ㄤ細璇濅腑鎵惧埌鍏?contextHistory 涓ユ牸浣滀负娑堟伅鍓嶇紑鐨?
// 閭ｄ釜浼氳瘽锛涘彧閫夊墠缂€鏈€闀跨殑涓€涓紝閬垮厤鐭墠缂€鍦ㄤ笉鍚屼細璇濋棿浜掓挒銆傝繑鍥?
// (sessionID, 鍖归厤鍒扮殑娑堟伅鏉℃暟)銆?
func (sr *sessionResolver) matchContextLocked(tenant, ipFinger string, messages []oaiMsg) (string, int) {
	if len(messages) == 0 {
		return "", 0
	}
	type match struct {
		id     string
		n      int
		recent time.Time
	}
	best := match{}
	for id, sess := range sr.sessions {
		if time.Since(sess.LastUsedAt) > sr.contextTTL {
			continue
		}
		if sess.Tenant != tenant {
			continue
		}
		if sess.IPFingerprint != ipFinger {
			continue
		}
		n := contextPrefixLen(sess.ContextHistory, messages)
		if n >= 1 && (n > best.n || (n == best.n && sess.LastUsedAt.After(best.recent))) {
			best = match{id: id, n: n, recent: sess.LastUsedAt}
		}
	}
	return best.id, best.n
}

// contextPrefixLen 杩斿洖 hist 鏄惁涓ユ牸鏄?msgs 鐨勫墠缂€銆俬ist 涓虹┖鎴栦笉鏄墠缂€
// 鏃惰繑鍥?0锛涘懡涓椂杩斿洖 len(hist)锛屽嵆澧為噺鍙戦€佽捣鐐广€?
// atom 杈圭晫妫€鏌ワ細hist 蹇呴』鍦?msgs 鐨勫師瀛愯竟鐣屼笂缁撴潫锛屽惁鍒欒涓洪潪鍘熷瓙鍒囧壊鑰岃繑鍥?0銆?
func contextPrefixLen(hist, msgs []oaiMsg) int {
	if len(hist) == 0 || len(msgs) < len(hist) {
		return 0
	}
	for i := range hist {
		if !messagesEqual(hist[i], msgs[i]) {
			return 0
		}
	}
	atoms := buildAtoms(msgs)
	boundary := false
	for _, a := range atoms {
		if a.End == len(hist) {
			boundary = true
			break
		}
		if a.End > len(hist) {
			break
		}
	}
	if !boundary {
		return 0
	}
	return len(hist)
}

// canonicalMessageHash hashes one message the same way messagesEqual compares
// it: role + content, with tool calls normalized through toolCallsFromMessage
// (structured tool_calls and text "CALL_TOOL: name({...})" produce the same
// hash, tool IDs ignored, JSON args key-order-insensitive). Message-level,
// so full-history hashes agree across representation changes.
func canonicalMessageHash(m oaiMsg) string {
	h := sha256.New()
	io.WriteString(h, m.Role)
	io.WriteString(h, "\x00")
	calls := toolCallsFromMessage(m)
	if len(calls) == 0 {
		// 空语义归一:与 messagesEqual 保持一致 —— "(empty)"、NO_TOOL_NEEDED、
		// 空白都算"无实质文本",否则同一轮重放哈希不同,同长度帧无法按
		// ContentHash 迁移(b8efef2f / 4653cac6 现场)。
		io.WriteString(h, normalizedEmptyishContent(m.Content))
		io.WriteString(h, "\x00")
		return hex.EncodeToString(h.Sum(nil))
	}
	for _, raw := range calls {
		fn, _ := raw["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		io.WriteString(h, "tool\x00")
		io.WriteString(h, name)
		io.WriteString(h, "\x00")
		io.WriteString(h, normalizeJSONArgs(args))
		io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// bindingContentHash is the canonical full-history fingerprint of a session's
// messages: each message hashed through canonicalMessageHash, concatenated.
// Two histories that messagesEqual would accept as equal — including text
// CALL_TOOL vs structured tool_calls — hash identically, so Bind can detect
// that a fresh upstream ConversationID belongs to an already-recorded logical
// conversation and migrate instead of forking.
func bindingContentHash(messages []oaiMsg) string {
	h := sha256.New()
	for _, m := range messages {
		io.WriteString(h, canonicalMessageHash(m))
		io.WriteString(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// historyPrefixEqual reports whether `prefix` equals the first len(prefix)
// messages of `history`, compared with messagesEqual semantics (so text
// CALL_TOOL and structured tool_calls match, tool IDs ignored, args
// key-order-insensitive). Used by Bind to detect that an existing binding's
// history is a strict prefix of the incoming full-context replay, proving the
// same logical thread continued rather than a fork.
func historyPrefixEqual(history, prefix []oaiMsg) bool {
	if len(prefix) == 0 || len(history) < len(prefix) {
		return false
	}
	for i := range prefix {
		if !messagesEqual(history[i], prefix[i]) {
			return false
		}
	}
	return true
}

// messageEqual 判断两条消息在会话键意义上等价:role 与文本内容一致。
// 忽略 tool_calls 的 ID 细节（会话键只关心内容如何被模型消化）。
// 工具调用存在两种等价形态：结构化 tool_calls（OpenAI 重放）与
// "CALL_TOOL: name({...})" 纯文本（上游模型文本决策/客户端改写重放）。
// 同一逻辑工具调用以不同形态出现在前后轮时视为相等，否则连续工具循环
// 的会话前缀永远无法匹配（每轮都新建 cloud conversation）。
func messagesEqual(a, b oaiMsg) bool {
	if a.Role != b.Role {
		return false
	}
	ha, hb := toolCallsFromMessage(a), toolCallsFromMessage(b)
	if len(ha) > 0 || len(hb) > 0 {
		if len(ha) != len(hb) {
			// 一边有工具调用另一边完全没有（纯文本对话 vs 工具链）必然不等，
			// 除非是 CALL_TOOL 文本形态 —— 该形态已由 toolCallsFromMessage 提取。
			return len(ha) == 0 && len(hb) == 0
		}
		for i := range ha {
			if !toolCallEqual(ha[i], hb[i]) {
				return false
			}
		}
		return true
	}
	// 两边都无工具调用:严格按内容比较。
	// 空语义归一:纯空 / "(empty)" / 独立 "NO_TOOL_NEEDED"(未经工具路由,
	// 或上游用它做终止标记)都是"本轮无实质文本"。同一轮重放时客户端可能
	// 一次写成 NO_TOOL_NEEDED、一次写成 empty 占位 —— 前缀继承必须把它们
	// 视为同一内容,否则同会话帧又分叉(b8efef2f / 4653cac6 现场)。
	if isEmptyishContent(a.Content) && isEmptyishContent(b.Content) {
		return true
	}
	return contentToString(a.Content) == contentToString(b.Content)
}

// isEmptyishContent 判断一条消息的内容是否"无实质文本":空串、纯空白、
// "(empty)" 占位、或独立 NO_TOOL_NEEDED 终止标记。这些形态在会话键语义上
// 等价——它们都表示"该轮没有给用户可读的输出"。
func isEmptyishContent(c any) bool {
	t := strings.TrimSpace(contentToString(c))
	if t == "" {
		return true
	}
	if t == "(empty)" || t == "NO_TOOL_NEEDED" {
		return true
	}
	return false
}

// normalizedEmptyishContent 返回参与哈希的规范化内容:空语义一律归一为
// 固定占位符,保证与 isEmptyishContent 判定完全一致(同一轮无论重放成
// 空串 / "(empty)" / NO_TOOL_NEEDED,哈希都相同)。
func normalizedEmptyishContent(c any) string {
	if isEmptyishContent(c) {
		return "\x00empty\x00"
	}
	return contentToString(c)
}

// toolCallsFromMessage 提取语义上的工具调用列表:结构化 tool_calls 原样返回;
// "CALL_TOOL: name({...})" 纯文本形态解析为等价工具调用。返回 nil 表示该消息
// 不携带任何工具调用（纯文本/普通轮次）。
func toolCallsFromMessage(m oaiMsg) []map[string]any {
	if len(m.ToolCalls) > 0 {
		return m.ToolCalls
	}
	if len(m.ToolCalls) == 0 && m.Role == "assistant" {
		if calls := parseCallToolText(contentToString(m.Content)); len(calls) > 0 {
			return calls
		}
	}
	return nil
}

// parseCallToolText 解析 "CALL_TOOL: name({...})" 文本，返回按 name+arguments
// 组织的工具调用列表（arguments 为原始 JSON 字符串）。
func parseCallToolText(text string) []map[string]any {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "CALL_TOOL:") && !strings.HasPrefix(t, "call_tool:") {
		return nil
	}
	rest := strings.TrimSpace(t[strings.Index(t, ":")+1:])
	start := strings.Index(rest, "(")
	end := strings.LastIndex(rest, ")")
	if start <= 0 || end <= start {
		return nil
	}
	name := strings.TrimSpace(rest[:start])
	args := strings.TrimSpace(rest[start+1 : end])
	if name == "" {
		return nil
	}
	return []map[string]any{{
		"function": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}}
}

// toolCallEqual 比较 name 与 arguments，忽略 ID：同一段工具调用重放时
// ID 由客户端重新生成，不应影响会话键。arguments 做 JSON 语义比较，
// 容忍键序差异（如 {"a":1,"b":2} 与 {"b":2,"a":1} 是同一调用）。
func toolCallEqual(x, y map[string]any) bool {
	xFunc, _ := x["function"].(map[string]any)
	yFunc, _ := y["function"].(map[string]any)
	xn, _ := xFunc["name"].(string)
	yn, _ := yFunc["name"].(string)
	if xn != yn {
		return false
	}
	xa, _ := xFunc["arguments"].(string)
	ya, _ := yFunc["arguments"].(string)
	return normalizeJSONArgs(xa) == normalizeJSONArgs(ya)
}

// normalizeJSONArgs 将参数 JSON 归一化（解析后重排），对非 JSON/无法解析的
// 原样返回，保证比较既容忍键序又不会把畸形参数误判为相等。
func normalizeJSONArgs(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(b)
}

func (sr *sessionResolver) Bind(sessionID, conversationID, accountID string, body *oaiReq, assistantText string, r *http.Request) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sr.evictLocked()

	tenant := tenantFromRequest(r)
	now := time.Now().UTC()
	history := cloneMessages(body.Messages)
	if strings.TrimSpace(assistantText) != "" {
		history = append(history, oaiMsg{Role: "assistant", Content: assistantText})
	}
	explicitID := r.Header.Get("X-M365-Session-Id")
	contentHash := bindingContentHash(history)

	// Locate an existing binding to update in place, scoped to this tenant:
	// prefer the tenant-namespaced explicit id, then any binding this tenant
	// already holds for the same cloud conversation. This keeps one record per
	// conversation instead of growing sessions.json on every incremental turn,
	// and never merges into another tenant's binding.
	targetKey := ""
	if explicitID != "" {
		if k, ok := sr.byExplicit[explicitKey(tenant, explicitID)]; ok {
			if sess, ok := sr.sessions[k]; ok && sess.Tenant == tenant {
				targetKey = k
			}
		}
	}
	if targetKey == "" && conversationID != "" {
		for k, sess := range sr.sessions {
			if sess.Tenant == tenant && sess.ConversationID == conversationID {
				targetKey = k
				break
			}
		}
	}
	// Content-key migration — two shapes of the same logical thread:
	//
	// 1) Exact-content rotation: the upstream M365 hands out a NEW cloud
	//    ConversationID for an IDENTICAL history replay (same messages, new
	//    cloud id). Bind adopts the existing binding by full canonical hash.
	// 2) Prefix-inheritance: the client replays a full-context history that
	//    GREW by a turn (9→11→13 messages; same logical thread, new cloud id).
	//    An existing binding whose history is a strict PREFIX of the incoming
	//    history proves the same thread continued — adopt it and migrate the
	//    new cloud id forward instead of forking one binding per frame.
	//
	// Without both, sessions.json fragments one conversation into multiple
	// bindings (observed: 5 bindings for one "修复tg不通" thread).
	if targetKey == "" && len(history) > 0 {
		bestKey, bestLen := "", 0
		for k, sess := range sr.sessions {
			if sess.Tenant != tenant {
				continue
			}
			sh := sess.ContextHistory
			if len(sh) == 0 {
				continue
			}
			if len(sh) == len(history) {
				// Exact-content rotation: same length, canonical hash equal.
				if sess.ContentHash != "" && sess.ContentHash == contentHash {
					bestKey, bestLen = k, len(sh)
					break
				}
				continue
			}
			if len(sh) >= len(history) {
				continue
			}
			// Strict prefix extension: incoming[:len(sh)] == sh.
			if !historyPrefixEqual(history, sh) {
				continue
			}
			if len(sh) > bestLen {
				bestKey, bestLen = k, len(sh)
			}
		}
		if bestKey != "" {
			targetKey = bestKey
		}
	}
	if targetKey != "" {
		sess := sr.sessions[targetKey]
		sess.ConversationID = conversationID
		sess.AccountID = accountID
		sess.LastUsedAt = now
		sess.UserField = body.User
		sess.IPFingerprint = clientIPFingerprint(r)
		sess.ContextFinger = contextFingerprint(history)
		sess.ContextHistory = history
		sess.ContentHash = contentHash
		sess.Tenant = tenant
		if explicitID != "" {
			sess.ExplicitID = explicitID
		}
		sr.sessions[targetKey] = sess
		sr.reindexLocked(sess)
		sr.persist.markDirty()
		return
	}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	sess := sessionBinding{
		SessionID:      sessionID,
		ConversationID: conversationID,
		AccountID:      accountID,
		CreatedAt:      now,
		LastUsedAt:     now,
		IPFingerprint:  clientIPFingerprint(r),
		UserField:      body.User,
		ContextFinger:  contextFingerprint(history),
		ContextHistory: history,
		ContentHash:    contentHash,
		Tenant:         tenant,
		ExplicitID:     explicitID,
	}
	sr.reindexLocked(sess)
	sr.persist.markDirty()
}

func (sr *sessionResolver) GetSession(tenant, sessionID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.lookupForTenantLocked(tenant, sessionID)
}

// lookupForTenantLocked resolves a client-facing id (either the raw SessionID
// or the tenant's explicit X-M365-Session-Id) to a binding this tenant owns.
func (sr *sessionResolver) lookupForTenantLocked(tenant, id string) (sessionBinding, bool) {
	if tenant == "" || id == "" {
		return sessionBinding{}, false
	}
	if s, ok := sr.sessions[id]; ok && s.Tenant == tenant {
		return s, true
	}
	if k, ok := sr.byExplicit[explicitKey(tenant, id)]; ok {
		if s, ok := sr.sessions[k]; ok && s.Tenant == tenant {
			return s, true
		}
	}
	return sessionBinding{}, false
}

func (sr *sessionResolver) GetConversation(conversationID string) (sessionBinding, bool) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	for _, session := range sr.sessions {
		if session.ConversationID == conversationID {
			session.ContextHistory = cloneMessages(session.ContextHistory)
			return session, true
		}
	}
	return sessionBinding{}, false
}

func (sr *sessionResolver) ListSessions() []sessionBinding {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]sessionBinding, 0, len(sr.sessions))
	for _, s := range sr.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastUsedAt.After(out[j].LastUsedAt)
	})
	return out
}

func (sr *sessionResolver) DeleteSession(tenant, sessionID string) bool {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	sess, ok := sr.lookupForTenantLocked(tenant, sessionID)
	if !ok {
		return false
	}
	sr.dropLocked(sess.SessionID, sess)
	sr.persist.markDirty()
	return true
}

// UnbindByConversation drops every session bound to the given conversation.
// Called after an automatic cleanup deletes the cloud conversation, so the
// anti-CrossID resolver never reuses a dead conversation.
func (sr *sessionResolver) UnbindByConversation(conversationID string) int {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	removed := 0
	for sid, s := range sr.sessions {
		if s.ConversationID != conversationID {
			continue
		}
		// dropLocked removes the binding and every derived index entry
		// (including the tenant-namespaced byExplicit key). This is a global
		// maintenance path: a deleted cloud conversation is unbound for every
		// tenant, so no tenant filter is applied here.
		sr.dropLocked(sid, s)
		removed++
	}
	if removed > 0 {
		sr.persist.markDirty()
	}
	return removed
}

func cloneMessages(msgs []oaiMsg) []oaiMsg {
	if len(msgs) <= 512 {
		out := make([]oaiMsg, len(msgs))
		copy(out, msgs)
		return out
	}
	atoms := buildAtoms(msgs)
	if len(atoms) == 0 {
		msgs = msgs[len(msgs)-512:]
		out := make([]oaiMsg, len(msgs))
		copy(out, msgs)
		return out
	}
	count := 0
	startIdx := len(msgs)
	for i := len(atoms) - 1; i >= 0; i-- {
		c := atoms[i].End - atoms[i].Start
		if count+c > 512 {
			break
		}
		count += c
		startIdx = atoms[i].Start
	}
	if count == 0 {
		startIdx = atoms[len(atoms)-1].Start
	}
	sliced := msgs[startIdx:]
	out := make([]oaiMsg, len(sliced))
	copy(out, sliced)
	return out
}

func explicitKey(tenant, id string) string { return tenant + "\x00" + id }

// tenantFromRequest derives a stable, non-reversible tenant identifier from the
// caller's API key so per-caller session state is isolated. Returns "" when no
// key is present; an empty tenant never matches a stored (keyed) binding.
func tenantFromRequest(r *http.Request) string {
	raw := rawAPIKey(r)
	if raw == "" {
		return ""
	}
	return keyHash(raw)
}

// ListSessionsForTenant returns only the bindings owned by the given tenant,
// most-recently-used first. Used by the API-key-authenticated /v1/sessions
// endpoint; the global ListSessions is reserved for admin/maintenance paths.
func (sr *sessionResolver) ListSessionsForTenant(tenant string) []sessionBinding {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	out := make([]sessionBinding, 0)
	if tenant == "" {
		return out
	}
	for _, s := range sr.sessions {
		if s.Tenant == tenant {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsedAt.After(out[j].LastUsedAt) })
	return out
}
