package main

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanSourceName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Logs/PC-XYZ-2024/Chrome_Default/Network/Cookies.txt", "PC-XYZ-2024_Chrome_Default"},
		{"ChromeDefault/Cookies.txt", "ChromeDefault"},
		{"FirefoxProfile/cookies.json", "FirefoxProfile"},
		{"Logs/[USA][Win10]hash123/Edge/Profile/cookies.txt", "USAWin10hash123_Edge"},
		{"random/cookies.txt", "random"},
		{"cookies.txt", "session"},

		// regression: the same wrapper folder must NOT collapse different
		// victims into the same name.
		{"DUMP_2024/[USA][Chrome]hash_aaa/Chrome_Default/Network/Cookies.txt",
			"DUMP_2024_USAChromehash_aaa_Chrome_Default"},
		{"DUMP_2024/[USA][Chrome]hash_bbb/Chrome_Default/Network/Cookies.txt",
			"DUMP_2024_USAChromehash_bbb_Chrome_Default"},
	}
	for _, c := range cases {
		got := cleanSourceName(c.in)
		if got != c.want {
			t.Errorf("cleanSourceName(%q):\n  got  %q\n  want %q", c.in, got, c.want)
		}
	}
}

// Regression: a wrapper folder shared by all victims must not collapse them
// into a single zip entry. With the old code the user reported one .txt
// holding cookies from all 17 victims.
func TestCleanSourceName_NoWrapperCollapse(t *testing.T) {
	wrapper := "DUMP_2024_05"
	names := map[string]bool{}
	for i := 0; i < 17; i++ {
		src := fmt.Sprintf("%s/[USA][Chrome]hash_%02d/Chrome_Default/Network/Cookies.txt", wrapper, i)
		names[cleanSourceName(src)] = true
	}
	if len(names) != 17 {
		t.Errorf("17 distinct victims collapsed to %d distinct names: %v", len(names), names)
	}
}

func TestStreamFilterToZip(t *testing.T) {
	dir := t.TempDir()
	spoolPath := filepath.Join(dir, "spool")

	sp, err := NewSpool(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	rows := []CookieRow{
		{Domain: ".netflix.com", Path: "/", Name: "id", Value: "n1", Source: "Logs/VICT-001/Chrome_Default/Network/Cookies.txt"},
		{Domain: ".paypal.com", Path: "/", Name: "sess", Value: "p1", Source: "Logs/VICT-001/Chrome_Default/Network/Cookies.txt"},
		{Domain: ".steam.com", Path: "/", Name: "tok", Value: "s1", Source: "Logs/VICT-002/Firefox/Profile/cookies.json"},
		{Domain: ".discord.com", Path: "/", Name: "auth", Value: "d1", Source: "Logs/VICT-001/Edge/Profile/Network/cookies.txt"},
	}
	for _, r := range rows {
		sp.OnCookieFile(r.Source)
		sp.Add(r)
	}
	sp.Close()

	zipPath := filepath.Join(dir, "out.zip")
	selected := map[string]bool{
		"netflix.com":  true,
		"steam.com":    true,
		"discord.com":  true,
	}
	written, total, err := streamFilterToZip(spoolPath, zipPath, selected, nil)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total rows: want 3, got %d", total)
	}
	if written != 3 {
		t.Errorf("sessions written: want 3 (one per victim+browser), got %d", written)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()

	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	t.Logf("zip entries: %v", names)

	wantContains := []string{"VICT-001_Chrome", "VICT-002_Firefox", "VICT-001_Edge"}
	for _, w := range wantContains {
		found := false
		for _, n := range names {
			if strings.Contains(n, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no zip entry contains %q (got %v)", w, names)
		}
	}
	for _, n := range names {
		if strings.Contains(n, "/") || strings.Contains(n, "\\") {
			t.Errorf("entry name still has path separators: %q", n)
		}
		if strings.Contains(n, "Cookies") || strings.Contains(n, "Network") {
			t.Errorf("entry name still contains noise: %q", n)
		}
	}
}

// Regression: filtered rows from many sources interleaved in the spool must
// all land in their own zip entries with full content. The previous
// implementation only kept the first row per source because archive/zip
// invalidates the previous entry's writer on each new Create call.
func TestStreamFilterToZip_InterleavedSources(t *testing.T) {
	dir := t.TempDir()
	spoolPath := filepath.Join(dir, "spool")
	sp, err := NewSpool(spoolPath)
	if err != nil {
		t.Fatal(err)
	}
	// 17 victims, 6 cookies each, all interleaved by domain to force
	// source-switching on every single line read during filter.
	const victims = 17
	const perVictim = 6
	for c := 0; c < perVictim; c++ {
		for v := 0; v < victims; v++ {
			sp.Add(CookieRow{
				Domain: ".cursor.com",
				Path:   "/",
				Name:   fmt.Sprintf("name_v%d_c%d", v, c),
				Value:  "v",
				Source: fmt.Sprintf("Logs/VICT-%02d/Chrome/Network/Cookies.txt", v),
			})
		}
	}
	sp.Close()

	zipPath := filepath.Join(dir, "out.zip")
	written, total, err := streamFilterToZip(spoolPath, zipPath, nil, []string{"cursor.com"})
	if err != nil {
		t.Fatal(err)
	}
	if written != victims {
		t.Errorf("sessions: want %d, got %d", victims, written)
	}
	if total != victims*perVictim {
		t.Errorf("total rows: want %d, got %d", victims*perVictim, total)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	if len(zr.File) != victims {
		t.Fatalf("zip entries: want %d, got %d", victims, len(zr.File))
	}
	for _, f := range zr.File {
		rc, _ := f.Open()
		buf := new(strings.Builder)
		io.Copy(buf, rc)
		rc.Close()
		// each entry must contain perVictim cookie lines (plus 3 header lines + blank)
		body := buf.String()
		dataLines := 0
		for _, ln := range strings.Split(body, "\n") {
			if ln != "" && !strings.HasPrefix(ln, "#") {
				dataLines++
			}
		}
		if dataLines != perVictim {
			t.Errorf("%s: want %d cookie lines, got %d\n%s", f.Name, perVictim, dataLines, body)
		}
	}
}

func TestSpoolDedupe(t *testing.T) {
	dir := t.TempDir()
	sp, err := NewSpool(filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	r := CookieRow{Domain: ".x.com", Path: "/", Name: "a", Value: "1", Source: "A/cookies.txt"}
	if !sp.Add(r) {
		t.Error("first Add should be new")
	}
	if sp.Add(r) {
		t.Error("dup Add should return false")
	}
	st := sp.Stats()
	if st.TotalCookies != 2 || st.UniqueCookies != 1 {
		t.Errorf("stats wrong: total=%d unique=%d (want 2/1)", st.TotalCookies, st.UniqueCookies)
	}
	sp.Close()

	rows, err := readSpool(filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("spool readback: want 1 row, got %d", len(rows))
	}
}

func TestSpoolScrubsBadChars(t *testing.T) {
	dir := t.TempDir()
	sp, _ := NewSpool(filepath.Join(dir, "s"))
	sp.Add(CookieRow{
		Domain: ".bad.com", Path: "/", Name: "n",
		Value:  "evil\x1fvalue\nwith\rnewlines",
		Source: "A/cookies.txt",
	})
	sp.Close()
	data, _ := os.ReadFile(filepath.Join(dir, "s"))
	if strings.Count(string(data), "\n") != 1 {
		t.Errorf("expected exactly 1 newline (record terminator), got: %q", string(data))
	}
}
