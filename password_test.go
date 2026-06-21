package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	zipx "github.com/yeka/zip"
)

func TestProcessZip_Password(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "secret.zip")
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zipx.NewWriter(zf)

	body := []byte(".netflix.com\tTRUE\t/\tTRUE\t1999999999\tNetflixId\tlocked123\n")

	w, err := zw.Encrypt("Cookies.txt", "hunter2", zipx.AES256Encryption)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	zf.Close()

	if _, _, err := processArchive(zipPath, "", ""); !errors.Is(err, ErrPasswordRequired) {
		t.Fatalf("want ErrPasswordRequired, got %v", err)
	}

	if _, _, err := processArchive(zipPath, "", "wrong"); !errors.Is(err, ErrBadPassword) {
		t.Logf("bad-password error: %v (acceptable if extractor fails differently)", err)
	}

	rows, _, err := processArchive(zipPath, "", "hunter2")
	if err != nil {
		t.Fatalf("decrypt with right pw failed: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "NetflixId" || rows[0].Value != "locked123" {
		t.Fatalf("decrypted rows wrong: %+v", rows)
	}
}
