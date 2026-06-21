package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRarVolumeIndex(t *testing.T) {
	cases := []struct {
		name string
		idx  int
		ok   bool
	}{
		{"logs.rar", 0, true},
		{"logs.part1.rar", 0, true},
		{"logs.part02.rar", 1, true},
		{"logs.part3.rar", 2, true},
		{"logs.r00", 1, true},
		{"logs.r01", 2, true},
		{"archive.001.rar", 0, true},
		{"readme.txt", 0, false},
	}
	for _, c := range cases {
		idx, ok := rarVolumeIndex(c.name)
		if ok != c.ok || (ok && idx != c.idx) {
			t.Errorf("rarVolumeIndex(%q) = (%d, %v), want (%d, %v)", c.name, idx, ok, c.idx, c.ok)
		}
	}
}

func TestResolveRarOpenPath(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("logs.part2.rar")
	_, err := resolveRarOpenPath(dir)
	if !errors.Is(err, ErrRarPartsMissing) {
		t.Fatalf("missing first part: want ErrRarPartsMissing, got %v", err)
	}

	write("logs.part1.rar")
	got, err := resolveRarOpenPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "__rarjoin.rar") && filepath.Base(got) != "logs.part1.rar" {
		t.Fatalf("open path = %q", got)
	}
}

func TestValidateRarVolumeSetMissingMiddle(t *testing.T) {
	vols := []rarVolumeCandidate{
		{path: "a.part1.rar", idx: 0},
		{path: "a.part3.rar", idx: 2},
	}
	err := validateRarVolumeSet(vols)
	if !errors.Is(err, ErrRarPartsMissing) {
		t.Fatalf("want ErrRarPartsMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "part2") {
		t.Fatalf("expected part2 in error: %v", err)
	}
}

func TestPrepareRarVolumesSpacedName(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("@ft7logs premium 17540.part1.rar")
	write("@ft7logs premium 17540.part2.rar")
	write("@ft7logs premium 17540.part3.rar")
	got, err := prepareRarVolumesForDecode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "__rarjoin.rar" {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "__rarjoin.r00")); err != nil {
		t.Fatalf("missing join r00: %v", err)
	}
}

func TestLooksLikeMultipartRar(t *testing.T) {
	if !looksLikeMultipartRar("dump.part1.rar") {
		t.Fatal("part1 should look multipart")
	}
	if looksLikeMultipartRar("dump.rar") {
		t.Fatal("plain rar should not look multipart")
	}
	if !looksLikeMultipartRar("dump.r00") {
		t.Fatal(".r00 should look multipart")
	}
}
