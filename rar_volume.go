package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	reRarPartNum = regexp.MustCompile(`(?i)(?:part|vol|volume)[._-]?(\d+)`)
	reRarDotNum  = regexp.MustCompile(`\.(\d+)\.rar$`)

	ErrRarPartsMissing = errors.New("rar multi-volume archive is missing earlier parts — send all .rar/.r00 parts to the bot, then /done")
)

func isRarContinuationExt(ext string) bool {
	if len(ext) != 4 || ext[0] != '.' || ext[1] != 'r' {
		return false
	}
	return ext[2] >= '0' && ext[2] <= '9' && ext[3] >= '0' && ext[3] <= '9'
}

func isArchiveUploadName(name string) bool {
	low := strings.ToLower(filepath.Base(name))
	if strings.HasSuffix(low, ".zip") || strings.HasSuffix(low, ".rar") {
		return true
	}
	return isRarContinuationExt(filepath.Ext(low))
}

func sanitizeArchiveFilename(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	base = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', 0:
			return -1
		default:
			return r
		}
	}, base)
	if base == "" || base == "." {
		return "archive"
	}
	return base
}

// rarVolumeIndex returns the 0-based volume index used by rardecode.
// Old-style naming: archive.rar → 0, archive.r00 → 1, archive.r01 → 2.
func rarVolumeIndex(name string) (int, bool) {
	low := strings.ToLower(filepath.Base(name))
	ext := filepath.Ext(low)
	if ext == ".rar" {
		stem := strings.TrimSuffix(low, ext)
		if m := reRarPartNum.FindStringSubmatch(stem); len(m) == 2 {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return 0, true
			}
			if n <= 0 {
				return 0, true
			}
			return n - 1, true
		}
		if m := reRarDotNum.FindStringSubmatch(low); len(m) == 2 {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				return 0, true
			}
			if n <= 0 {
				return 0, true
			}
			return n - 1, true
		}
		return 0, true
	}
	if isRarContinuationExt(ext) {
		n, err := strconv.Atoi(ext[2:])
		if err != nil {
			return 0, false
		}
		return n + 1, true
	}
	return 0, false
}

func looksLikeMultipartRar(name string) bool {
	idx, ok := rarVolumeIndex(name)
	if !ok {
		return false
	}
	if idx > 0 {
		return true
	}
	low := strings.ToLower(filepath.Base(name))
	if reRarPartNum.MatchString(low) {
		return true
	}
	return isRarContinuationExt(filepath.Ext(low))
}

type rarVolumeCandidate struct {
	path string
	idx  int
}

func listRarVolumes(dir string) ([]rarVolumeCandidate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []rarVolumeCandidate
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		idx, ok := rarVolumeIndex(name)
		if !ok {
			continue
		}
		out = append(out, rarVolumeCandidate{
			path: filepath.Join(dir, name),
			idx:  idx,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].idx != out[j].idx {
			return out[i].idx < out[j].idx
		}
		return out[i].path < out[j].path
	})
	return out, nil
}

func resolveRarOpenPath(dir string) (string, error) {
	vols, err := listRarVolumes(dir)
	if err != nil {
		return "", err
	}
	if len(vols) == 0 {
		return "", fmt.Errorf("no rar volumes found in %s", dir)
	}
	first := vols[0]
	if first.idx > 0 {
		return "", fmt.Errorf("%w (have %s, need volume 1/first part)", ErrRarPartsMissing, filepath.Base(first.path))
	}
	return first.path, nil
}

func removeArchiveFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		if isArchiveUploadName(ent.Name()) {
			os.Remove(filepath.Join(dir, ent.Name()))
		}
	}
}
