package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestProcessZip_Smoke(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "sample.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)

	netscape := "# Netscape HTTP Cookie File\n" +
		".netflix.com\tTRUE\t/\tTRUE\t1999999999\tNetflixId\tabc123\n" +
		"#HttpOnly_.steamcommunity.com\tTRUE\t/\tTRUE\t1999999999\tsteamLoginSecure\txyz\n" +
		".netflix.com\tTRUE\t/\tTRUE\t1999999999\tNetflixId\tabc123\n"

	jsonCookies := `[{"domain":".discord.com","path":"/","name":"token","value":"mfa.xyz","secure":true,"hostOnly":false,"expirationDate":1999999999}]`

	files := map[string]string{
		"ChromeDefault/Cookies.txt":   netscape,
		"FirefoxProfile/cookies.json": jsonCookies,
		"random/readme.txt":           "not a cookie file",
	}
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	zf.Close()

	rows, stats, err := processArchive(zipPath, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// spool dedupes at write time, so we get 3 rows back.
	if len(rows) != 3 {
		t.Errorf("rows: want 3 (deduped at extract), got %d", len(rows))
	}
	if stats.TotalCookies != 4 {
		t.Errorf("TotalCookies: want 4 (pre-dedupe), got %d", stats.TotalCookies)
	}
	if stats.UniqueCookies != 3 {
		t.Errorf("UniqueCookies: want 3, got %d", stats.UniqueCookies)
	}
	if stats.CookieFiles != 2 {
		t.Errorf("cookie files: want 2, got %d", stats.CookieFiles)
	}

	rows2, _, _ := processArchive(zipPath, "netflix", "")
	if len(rows2) != 1 {
		t.Errorf("filtered rows: want 1, got %d", len(rows2))
	}
	if rows2[0].Domain != ".netflix.com" {
		t.Errorf("filter domain: got %q", rows2[0].Domain)
	}

	out := filepath.Join(dir, "cookies.txt")
	if err := writeNetscape(out, rows); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if len(data) < 50 {
		t.Errorf("netscape file too small: %d bytes", len(data))
	}
	t.Logf("netscape output:\n%s", string(data))
	t.Logf("stats: %+v", stats)
}
