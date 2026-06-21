package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/nwaples/rardecode/v2"
	zipx "github.com/yeka/zip"
)

// processArchive is the legacy in-memory wrapper kept for tests/CLI.
// Production uses processArchiveSpool directly to avoid materializing
// every cookie in RAM.
func processArchive(archivePath, filter, password string) ([]CookieRow, Stats, error) {
	tmpDir, err := os.MkdirTemp("", "spool-*")
	if err != nil {
		return nil, Stats{}, err
	}
	defer os.RemoveAll(tmpDir)
	spool, err := NewSpool(filepath.Join(tmpDir, "s"))
	if err != nil {
		return nil, Stats{}, err
	}
	perr := processArchiveSpool(archivePath, filter, password, 0, spool)
	spool.Close()
	stats := spool.Stats()
	if perr != nil {
		return nil, stats, perr
	}
	rows, rerr := readSpool(spool.Path())
	return rows, stats, rerr
}

var ErrPasswordRequired = errors.New("password required")
var ErrBadPassword = errors.New("bad password")

const MAX_NEST_DEPTH = 4

var (
	extractEntries int64
	extractCookies int64
)

func ResetExtractProgress() {
	atomic.StoreInt64(&extractEntries, 0)
	atomic.StoreInt64(&extractCookies, 0)
}

func ExtractProgress() (entries, cookies int64) {
	return atomic.LoadInt64(&extractEntries), atomic.LoadInt64(&extractCookies)
}

// processArchiveSpool is the streaming entry point — every cookie row is
// pushed to the spool (disk) instead of accumulated in a slice.
func processArchiveSpool(archivePath, filter, password string, depth int, spool *Spool) error {
	low := strings.ToLower(archivePath)
	if strings.HasSuffix(low, ".rar") {
		return processRarSpool(archivePath, filter, password, depth, spool)
	}
	return processZipSpool(archivePath, filter, password, depth, spool)
}

func isNestedArchive(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".zip") || strings.HasSuffix(n, ".rar")
}

func spawnNestedSpool(reader io.Reader, name string, filter, password string, depth int, spool *Spool) error {
	if depth+1 >= MAX_NEST_DEPTH {
		return nil
	}
	tmp, err := os.CreateTemp("", "nest-*"+filepath.Ext(name))
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, io.LimitReader(reader, MAX_ARCHIVE_BYTES)); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	return processArchiveSpool(tmpPath, filter, password, depth+1, spool)
}

func processZipSpool(zipPath, filter, password string, depth int, spool *Spool) error {
	zr, err := zipx.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	// Only require password if cookie files themselves are encrypted.
	// Non-cookie encrypted entries (Passwords.txt, SystemInfo.txt, etc.)
	// are skipped silently — don't bail on the whole archive for them.
	if password == "" {
		for _, zf := range zr.File {
			if !zf.FileInfo().IsDir() && zf.IsEncrypted() && looksLikeCookieFile(zf.Name) {
				return ErrPasswordRequired
			}
		}
	}

	flow := strings.ToLower(filter)

	for _, zf := range zr.File {
		spool.OnEntry()
		atomic.AddInt64(&extractEntries, 1)
		if zf.FileInfo().IsDir() {
			continue
		}
		name := zf.Name

		if isNestedArchive(name) && depth+1 < MAX_NEST_DEPTH {
			if zf.IsEncrypted() {
				if password == "" {
					continue // skip encrypted nested archives we can't open
				}
				zf.SetPassword(password)
			}
			rc, oerr := zf.Open()
			if oerr != nil {
				continue
			}
			_ = spawnNestedSpool(rc, name, filter, password, depth, spool)
			rc.Close()
			continue
		}

		if int64(zf.UncompressedSize64) > MAX_FILE_BYTES {
			continue
		}
		if !looksLikeCookieFile(name) {
			continue
		}

		if zf.IsEncrypted() {
			if password == "" {
				continue // already asked above — skip remaining encrypted cookie files
			}
			zf.SetPassword(password)
		}

		f, err := zf.Open()
		if err != nil {
			if isPasswordErr(err) {
				if password == "" {
					continue
				}
				return ErrBadPassword
			}
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(f, MAX_FILE_BYTES))
		f.Close()
		if rerr != nil {
			if isPasswordErr(rerr) {
				return ErrBadPassword
			}
			continue
		}

		parsed := parseCookieFile(name, data)
		data = nil
		if len(parsed) == 0 {
			continue
		}
		spool.OnCookieFile(name)

		for _, r := range parsed {
			if flow != "" && !strings.Contains(strings.ToLower(r.Domain), flow) {
				continue
			}
			if spool.Add(r) {
				atomic.AddInt64(&extractCookies, 1)
			}
		}
	}
	return nil
}

func processRarSpool(rarPath, filter, password string, depth int, spool *Spool) error {
	openPath := rarPath
	if resolved, err := resolveRarOpenPath(filepath.Dir(rarPath)); err == nil {
		openPath = resolved
	} else if errors.Is(err, ErrRarPartsMissing) {
		return err
	}

	opts := []rardecode.Option{}
	if password != "" {
		opts = append(opts, rardecode.Password(password))
	}
	r, err := rardecode.OpenReader(openPath, opts...)
	if err != nil {
		if errors.Is(err, rardecode.ErrBadPassword) || isPasswordErr(err) {
			if password == "" {
				return ErrPasswordRequired
			}
			return ErrBadPassword
		}
		return err
	}
	defer r.Close()

	flow := strings.ToLower(filter)

	for {
		hdr, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, rardecode.ErrBadPassword) || isPasswordErr(err) {
				if password == "" {
					return ErrPasswordRequired
				}
				return ErrBadPassword
			}
			if errors.Is(err, rardecode.ErrBadVolumeNumber) {
				return fmt.Errorf("%w (open the first part, e.g. .part1.rar or the main .rar)", ErrRarPartsMissing)
			}
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w (missing a continuation part)", ErrRarPartsMissing)
			}
			return err
		}
		spool.OnEntry()
		atomic.AddInt64(&extractEntries, 1)
		if hdr.IsDir {
			continue
		}
		name := hdr.Name

		if isNestedArchive(name) && depth+1 < MAX_NEST_DEPTH {
			if (hdr.Encrypted || hdr.HeaderEncrypted) && password == "" {
				continue // skip encrypted nested archive
			}
			_ = spawnNestedSpool(r, name, filter, password, depth, spool)
			continue
		}

		// Only block on encryption for cookie files — skip encrypted non-cookie entries.
		if (hdr.Encrypted || hdr.HeaderEncrypted) && password == "" {
			if looksLikeCookieFile(name) {
				return ErrPasswordRequired
			}
			continue
		}

		if hdr.UnPackedSize > MAX_FILE_BYTES {
			continue
		}
		if !looksLikeCookieFile(name) {
			continue
		}

		data, err := io.ReadAll(io.LimitReader(r, MAX_FILE_BYTES))
		if err != nil {
			if errors.Is(err, rardecode.ErrBadPassword) {
				if password == "" {
					return ErrPasswordRequired
				}
				return ErrBadPassword
			}
			continue
		}

		parsed := parseCookieFile(name, data)
		data = nil
		if len(parsed) == 0 {
			continue
		}
		spool.OnCookieFile(name)

		for _, cr := range parsed {
			if flow != "" && !strings.Contains(strings.ToLower(cr.Domain), flow) {
				continue
			}
			if spool.Add(cr) {
				atomic.AddInt64(&extractCookies, 1)
			}
		}
	}
	return nil
}

func isPasswordErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "password") || strings.Contains(s, "decryption") || strings.Contains(s, "encrypted")
}
