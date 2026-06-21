package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nwaples/rardecode/v2"
)

var (
	reRarPartNum = regexp.MustCompile(`(?i)(?:part|vol|volume)[._-]?(\d+)`)
	reRarDotNum  = regexp.MustCompile(`\.(\d+)\.rar$`)

	ErrRarPartsMissing = errors.New("rar multi-volume archive is missing earlier parts — send all .rar/.r00 parts to the bot, then /done")
)

const rarJoinBase = "__rarjoin"

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
		if strings.HasPrefix(name, rarJoinBase) {
			continue
		}
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

func formatRarVolumeList(vols []rarVolumeCandidate) string {
	names := make([]string, len(vols))
	for i, v := range vols {
		names[i] = filepath.Base(v.path)
	}
	return strings.Join(names, "`, `")
}

func validateRarVolumeSet(vols []rarVolumeCandidate) error {
	if len(vols) == 0 {
		return fmt.Errorf("no rar parts found — send .rar / .part1.rar / .r00 files, then /done")
	}
	if vols[0].idx > 0 {
		return fmt.Errorf("%w\nhave: `%s`\nmissing: `part1` / first `.rar` volume — send part 1 first, then the rest, then /done",
			ErrRarPartsMissing, formatRarVolumeList(vols))
	}
	seen := map[int]string{}
	maxIdx := 0
	for _, v := range vols {
		if prev, ok := seen[v.idx]; ok {
			return fmt.Errorf("%w\nduplicate volume %d: `%s` and `%s`",
				ErrRarPartsMissing, v.idx+1, filepath.Base(prev), filepath.Base(v.path))
		}
		seen[v.idx] = v.path
		if v.idx > maxIdx {
			maxIdx = v.idx
		}
	}
	var missing []string
	for i := 0; i <= maxIdx; i++ {
		if _, ok := seen[i]; !ok {
			missing = append(missing, fmt.Sprintf("part%d", i+1))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w\nhave: `%s`\nmissing: `%s` — send every part, then /done",
			ErrRarPartsMissing, formatRarVolumeList(vols), strings.Join(missing, "`, `"))
	}
	return nil
}

func linkOrCopy(src, dst string) error {
	_ = os.Remove(dst)
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func removeRarJoinArtifacts(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, rarJoinBase+".*"))
	for _, m := range matches {
		os.Remove(m)
	}
}

// prepareRarVolumesForDecode validates the part set and, for multi-part archives,
// links/copies volumes to archive.rar + .r00/.r01 names that rardecode reliably opens.
func prepareRarVolumesForDecode(dir string) (string, error) {
	vols, err := listRarVolumes(dir)
	if err != nil {
		return "", err
	}
	if err := validateRarVolumeSet(vols); err != nil {
		return "", err
	}
	if len(vols) == 1 {
		return vols[0].path, nil
	}

	removeRarJoinArtifacts(dir)
	base := filepath.Join(dir, rarJoinBase)
	if err := linkOrCopy(vols[0].path, base+".rar"); err != nil {
		return "", fmt.Errorf("prepare rar volume 1: %w", err)
	}
	for i := 1; i < len(vols); i++ {
		dst := fmt.Sprintf("%s.r%02d", base, i-1)
		if err := linkOrCopy(vols[i].path, dst); err != nil {
			return "", fmt.Errorf("prepare rar volume %d: %w", i+1, err)
		}
	}
	return base + ".rar", nil
}

func resolveRarOpenPath(dir string) (string, error) {
	return prepareRarVolumesForDecode(dir)
}

func removeArchiveFiles(dir string) {
	removeRarJoinArtifacts(dir)
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

// archiveExpectsMoreVolumes reports whether a lone .rar still needs continuation files.
func archiveExpectsMoreVolumes(path string) bool {
	rc, err := rardecode.OpenReader(path)
	if err != nil {
		return false
	}
	defer rc.Close()
	for {
		_, err := rc.Next()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return false
		}
		if errors.Is(err, rardecode.ErrMultiVolume) {
			return true
		}
		if errors.Is(err, fs.ErrNotExist) {
			return true
		}
		return false
	}
}
