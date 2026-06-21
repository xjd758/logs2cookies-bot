package main

import (
        "archive/zip"
        "bufio"
        "bytes"
        "fmt"
        "io"
        "os"
        "path/filepath"
        "sort"
        "strings"
        "sync"
)

const (
        SPOOL_FS = '\x1f'
        SPOOL_RS = '\n'
)

// Spool streams parsed cookies to a single on-disk file during extraction.
// Only stats + a dedupe set live in RAM, so a 5GB archive with millions of
// cookies costs ~MB of RAM instead of ~GB.
type Spool struct {
        mu    sync.Mutex
        path  string
        f     *os.File
        w     *bufio.Writer
        seen  map[string]struct{}
        stats Stats
}

func NewSpool(path string) (*Spool, error) {
        f, err := os.Create(path)
        if err != nil {
                return nil, err
        }
        return &Spool{
                path: path,
                f:    f,
                w:    bufio.NewWriterSize(f, 256*1024),
                seen: make(map[string]struct{}, 1<<14),
                stats: Stats{
                        DomainCounts: map[string]int{},
                        BrowserHints: map[string]int{},
                },
        }, nil
}

func (s *Spool) Path() string { return s.path }

func (s *Spool) OnEntry() {
        s.mu.Lock()
        s.stats.ArchiveFiles++
        s.mu.Unlock()
}

func (s *Spool) OnCookieFile(sourceName string) {
        s.mu.Lock()
        s.stats.CookieFiles++
        s.stats.BrowserHints[detectBrowser(sourceName)]++
        s.mu.Unlock()
}

// Add appends a cookie. Returns true if it was new (not a dup).
// Stats counts mirror the original (pre-dedupe) extractor's behavior:
// TotalCookies counts every call, DomainCounts also counts every call.
func (s *Spool) Add(r CookieRow) bool {
        s.mu.Lock()
        defer s.mu.Unlock()

        s.stats.TotalCookies++
        s.stats.DomainCounts[normalDomain(r.Domain)]++

        // Source is included so the same cookie value from two different victims
        // is NOT treated as a duplicate — each victim gets their own file entry.
        key := r.Source + "\x00" + r.Domain + "\x00" + r.Path + "\x00" + r.Name + "\x00" + r.Value
        if _, ok := s.seen[key]; ok {
                return false
        }
        s.seen[key] = struct{}{}
        s.stats.UniqueCookies++

        w := s.w
        w.WriteString(scrubField(r.Domain))
        w.WriteByte(SPOOL_FS)
        w.WriteString(scrubField(r.Flag))
        w.WriteByte(SPOOL_FS)
        w.WriteString(scrubField(r.Path))
        w.WriteByte(SPOOL_FS)
        w.WriteString(scrubField(r.Secure))
        w.WriteByte(SPOOL_FS)
        w.WriteString(scrubField(r.Expiration))
        w.WriteByte(SPOOL_FS)
        w.WriteString(scrubField(r.Name))
        w.WriteByte(SPOOL_FS)
        w.WriteString(scrubField(r.Value))
        w.WriteByte(SPOOL_FS)
        w.WriteString(scrubField(r.Source))
        w.WriteByte(SPOOL_RS)
        return true
}

// Stats returns a snapshot copy.
func (s *Spool) Stats() Stats {
        s.mu.Lock()
        defer s.mu.Unlock()
        dc := make(map[string]int, len(s.stats.DomainCounts))
        for k, v := range s.stats.DomainCounts {
                dc[k] = v
        }
        bh := make(map[string]int, len(s.stats.BrowserHints))
        for k, v := range s.stats.BrowserHints {
                bh[k] = v
        }
        return Stats{
                ArchiveFiles:  s.stats.ArchiveFiles,
                CookieFiles:   s.stats.CookieFiles,
                TotalCookies:  s.stats.TotalCookies,
                UniqueCookies: s.stats.UniqueCookies,
                DomainCounts:  dc,
                BrowserHints:  bh,
        }
}

// FreeSeen drops the dedupe set after extraction is done. Call before the
// long selector wait so the GC can reclaim it.
func (s *Spool) FreeSeen() {
        s.mu.Lock()
        s.seen = nil
        s.mu.Unlock()
}

func (s *Spool) Close() error {
        s.mu.Lock()
        defer s.mu.Unlock()
        if s.w != nil {
                s.w.Flush()
                s.w = nil
        }
        if s.f != nil {
                err := s.f.Close()
                s.f = nil
                return err
        }
        return nil
}

func scrubField(v string) string {
        if v == "" {
                return ""
        }
        if !strings.ContainsAny(v, "\n\r\x1f") {
                return v
        }
        r := strings.NewReplacer("\n", " ", "\r", " ", "\x1f", " ")
        return r.Replace(v)
}

// readSpool decodes the spool file into rows. Used only by the legacy
// processArchive() wrapper for tests — production never loads the whole spool.
func readSpool(path string) ([]CookieRow, error) {
        f, err := os.Open(path)
        if err != nil {
                return nil, err
        }
        defer f.Close()
        var rows []CookieRow
        sc := bufio.NewScanner(f)
        sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
        for sc.Scan() {
                line := sc.Bytes()
                parts := bytes.SplitN(line, []byte{SPOOL_FS}, 8)
                if len(parts) < 8 {
                        continue
                }
                rows = append(rows, CookieRow{
                        Domain:     string(parts[0]),
                        Flag:       string(parts[1]),
                        Path:       string(parts[2]),
                        Secure:     string(parts[3]),
                        Expiration: string(parts[4]),
                        Name:       string(parts[5]),
                        Value:      string(parts[6]),
                        Source:     string(parts[7]),
                })
        }
        return rows, sc.Err()
}

// streamFilterToZip reads spool line-by-line, applies selection, and writes
// one Netscape `.txt` per cleanSourceName into a zip.
//
// Why per-source temp files: archive/zip only allows ONE open entry at a
// time — calling Create on a new entry implicitly closes the previous one,
// so interleaved writers (which is what stream-filtering produces) corrupt
// or drop data. We spool filtered rows to per-source temp files first, then
// build the zip linearly at the end. RAM stays bounded by:
//   selected_unique_sources * (one bufio buffer + one *os.File handle)
func streamFilterToZip(spoolPath, zipPath string, selected map[string]bool, custom []string) (sessionsWritten int, totalRows int, err error) {
        sf, err := os.Open(spoolPath)
        if err != nil {
                return 0, 0, err
        }
        defer sf.Close()

        workDir := filepath.Dir(zipPath)

        type sink struct {
                f      *os.File
                bw     *bufio.Writer
                source string
                rows   int
        }
        sinks := map[string]*sink{}
        cleanupTemps := func() {
                for _, sk := range sinks {
                        sk.bw.Flush()
                        sk.f.Close()
                        os.Remove(sk.f.Name())
                }
        }

        sc := bufio.NewScanner(sf)
        sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

        for sc.Scan() {
                line := sc.Bytes()
                if len(line) == 0 {
                        continue
                }
                parts := bytes.SplitN(line, []byte{SPOOL_FS}, 8)
                if len(parts) < 8 {
                        continue
                }
                dom := normalDomainBytes(parts[0])
                match := selected[dom]
                if !match {
                        for _, sub := range custom {
                                if strings.Contains(dom, sub) {
                                        match = true
                                        break
                                }
                        }
                }
                if !match {
                        continue
                }

                source := string(parts[7])
                key := cleanSourceName(source)
                sk, ok := sinks[key]
                if !ok {
                        tmpf, terr := os.CreateTemp(workDir, "src-*.txt")
                        if terr != nil {
                                cleanupTemps()
                                return 0, totalRows, terr
                        }
                        bw := bufio.NewWriterSize(tmpf, 32*1024)
                        fmt.Fprintln(bw, "# Netscape HTTP Cookie File")
                        fmt.Fprintln(bw, "# Generated by logs2cookies-bot")
                        fmt.Fprintf(bw, "# Source: %s\n\n", source)
                        sk = &sink{f: tmpf, bw: bw, source: source}
                        sinks[key] = sk
                }
                fmt.Fprintf(sk.bw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
                        string(parts[0]),
                        defaultStr(string(parts[1]), "TRUE"),
                        defaultStr(string(parts[2]), "/"),
                        defaultStr(string(parts[3]), "FALSE"),
                        defaultStr(string(parts[4]), "0"),
                        string(parts[5]),
                        string(parts[6]),
                )
                sk.rows++
                totalRows++
        }
        if scErr := sc.Err(); scErr != nil {
                cleanupTemps()
                return 0, totalRows, scErr
        }

        // Build the zip from the temp files — one open entry at a time.
        zf, err := os.Create(zipPath)
        if err != nil {
                cleanupTemps()
                return 0, totalRows, err
        }
        defer zf.Close()
        zw := zip.NewWriter(zf)

        keys := make([]string, 0, len(sinks))
        for k := range sinks {
                keys = append(keys, k)
        }
        sort.Strings(keys)

        for _, key := range keys {
                sk := sinks[key]
                if err := sk.bw.Flush(); err != nil {
                        cleanupTemps()
                        zw.Close()
                        return 0, totalRows, err
                }
                if _, err := sk.f.Seek(0, 0); err != nil {
                        cleanupTemps()
                        zw.Close()
                        return 0, totalRows, err
                }
                ew, werr := zw.Create(key + ".txt")
                if werr != nil {
                        cleanupTemps()
                        zw.Close()
                        return 0, totalRows, werr
                }
                if _, err := io.Copy(ew, sk.f); err != nil {
                        cleanupTemps()
                        zw.Close()
                        return 0, totalRows, err
                }
        }
        if err := zw.Close(); err != nil {
                cleanupTemps()
                return 0, totalRows, err
        }
        cleanupTemps()
        return len(sinks), totalRows, nil
}

func normalDomainBytes(b []byte) string {
        if len(b) > 0 && b[0] == '.' {
                b = b[1:]
        }
        return strings.ToLower(string(b))
}
