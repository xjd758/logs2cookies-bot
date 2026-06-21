package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DEFAULT_PARALLEL  = 8
	DOWNLOAD_UA       = "logs2cookies/1.0 (go)"
	CHUNK_MIN_BYTES   = 10 * 1024 * 1024
	HTTP_DIAL_TIMEOUT = 30 * time.Second
)

type ProgressFn func(downloaded, total int64, speedBps float64)

type DownloadResult struct {
	Path               string
	Bytes              int64
	Duration           time.Duration
	Parallel           int
	RangeUsed          bool
	ContentType        string
	ContentDisposition string
	FileName           string
	FinalURL           string
}

func defaultHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DisableCompression:    true,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			MaxConnsPerHost:       32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 5 * time.Second,
		},
		Timeout: 0,
	}
}

type downloadMeta struct {
	size         int64
	acceptRanges bool
	contentType  string
	disposition  string
	filename     string
	finalURL     string
}

// fetchDownloadMeta tries HEAD first, then a 1-byte ranged GET probe.
// Many streaming mirrors (Railway, CDN passthrough, etc.) return 404 on HEAD
// but serve 206 on GET with Range — probe recovers size + range support.
func fetchDownloadMeta(ctx context.Context, client *http.Client, url string) (downloadMeta, error) {
	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return downloadMeta{}, err
	}
	headReq.Header.Set("User-Agent", DOWNLOAD_UA)
	headReq.Header.Set("Accept-Encoding", "identity")
	headResp, err := client.Do(headReq)
	if err == nil {
		headResp.Body.Close()
		if headResp.StatusCode/100 == 2 {
			cdisp := headResp.Header.Get("Content-Disposition")
			return downloadMeta{
				size:         headResp.ContentLength,
				acceptRanges: headResp.Header.Get("Accept-Ranges") == "bytes",
				contentType:  headResp.Header.Get("Content-Type"),
				disposition:  cdisp,
				filename:     filenameFromDisposition(cdisp),
				finalURL:     headResp.Request.URL.String(),
			}, nil
		}
		if isCloudflareChallenge(headResp.Header) {
			return downloadMeta{}, errors.New("cloudflare challenge blocked this download; paste the direct final .zip/.rar URL")
		}
	}
	// HEAD failed or returned non-2xx — probe with ranged GET.
	meta, probeErr := probeDownloadMeta(ctx, client, url)
	if probeErr == nil {
		return meta, nil
	}
	if err != nil {
		return downloadMeta{}, fmt.Errorf("HEAD: %w", err)
	}
	if headResp.StatusCode == http.StatusMethodNotAllowed || headResp.StatusCode == http.StatusForbidden {
		return downloadMeta{finalURL: headResp.Request.URL.String()}, nil
	}
	return downloadMeta{}, fmt.Errorf("HEAD status %d (range probe: %v)", headResp.StatusCode, probeErr)
}

func probeDownloadMeta(ctx context.Context, client *http.Client, url string) (downloadMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return downloadMeta{}, err
	}
	req.Header.Set("User-Agent", DOWNLOAD_UA)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", "bytes=0-0")

	resp, err := client.Do(req)
	if err != nil {
		return downloadMeta{}, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))

	if isCloudflareChallenge(resp.Header) {
		return downloadMeta{}, errors.New("cloudflare challenge blocked this download; paste the direct final .zip/.rar URL")
	}

	cdisp := resp.Header.Get("Content-Disposition")
	meta := downloadMeta{
		contentType: resp.Header.Get("Content-Type"),
		disposition: cdisp,
		filename:    filenameFromDisposition(cdisp),
		finalURL:    resp.Request.URL.String(),
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		size, err := parseContentRangeTotal(resp.Header.Get("Content-Range"))
		if err != nil {
			return downloadMeta{}, err
		}
		meta.size = size
		meta.acceptRanges = resp.Header.Get("Accept-Ranges") == "bytes" || resp.Header.Get("Content-Range") != ""
		return meta, nil
	case http.StatusOK:
		meta.size = resp.ContentLength
		return meta, nil
	default:
		return downloadMeta{}, fmt.Errorf("probe status %d", resp.StatusCode)
	}
}

func parseContentRangeTotal(v string) (int64, error) {
	if v == "" {
		return -1, errors.New("missing Content-Range")
	}
	i := strings.LastIndex(v, "/")
	if i < 0 {
		return -1, fmt.Errorf("bad Content-Range: %q", v)
	}
	total := strings.TrimSpace(v[i+1:])
	if total == "*" {
		return -1, nil
	}
	return strconv.ParseInt(total, 10, 64)
}

func parallelDownload(ctx context.Context, url, dst string, parallel int, maxBytes int64, prog ProgressFn) (*DownloadResult, error) {
	if parallel <= 0 {
		parallel = DEFAULT_PARALLEL
	}
	client := defaultHTTPClient()

	meta, err := fetchDownloadMeta(ctx, client, url)
	if err != nil {
		return nil, err
	}
	size := meta.size
	acceptRanges := meta.acceptRanges
	ctype := meta.contentType
	cdisp := meta.disposition
	filename := meta.filename
	finalURL := meta.finalURL
	if maxBytes > 0 && size > maxBytes {
		return nil, fmt.Errorf("file too large (%s, cap %s)", formatBytes(size), formatBytes(maxBytes))
	}

	if !acceptRanges || size <= 0 || size < CHUNK_MIN_BYTES {
		return singleStreamDownload(ctx, client, url, dst, size, ctype, cdisp, maxBytes, prog)
	}

	chunkSize := size / int64(parallel)
	if chunkSize < CHUNK_MIN_BYTES {
		parallel = int(size / CHUNK_MIN_BYTES)
		if parallel < 1 {
			parallel = 1
		}
		chunkSize = size / int64(parallel)
	}

	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, err
	}

	var downloaded int64
	start := time.Now()

	stopProg := make(chan struct{})
	if prog != nil {
		go progLoop(stopProg, &downloaded, size, start, prog)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, parallel)
	for i := 0; i < parallel; i++ {
		from := int64(i) * chunkSize
		to := from + chunkSize - 1
		if i == parallel-1 {
			to = size - 1
		}
		wg.Add(1)
		go func(idx int, from, to int64) {
			defer wg.Done()
			if err := downloadChunk(ctx, client, url, f, from, to, &downloaded); err != nil {
				errCh <- fmt.Errorf("chunk %d (%d-%d): %w", idx, from, to, err)
			}
		}(i, from, to)
	}
	wg.Wait()
	close(stopProg)
	close(errCh)
	for e := range errCh {
		f.Close()
		os.Remove(dst)
		return nil, e
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	return &DownloadResult{
		Path:               dst,
		Bytes:              size,
		Duration:           time.Since(start),
		Parallel:           parallel,
		RangeUsed:          true,
		ContentType:        ctype,
		ContentDisposition: cdisp,
		FileName:           filename,
		FinalURL:           finalURL,
	}, nil
}

func progLoop(stop chan struct{}, counter *int64, total int64, start time.Time, prog ProgressFn) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			d := atomic.LoadInt64(counter)
			prog(d, total, float64(d)/time.Since(start).Seconds())
		}
	}
}

func isRetryableStatus(code int) bool {
	switch code {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	return false
}

func backoff(attempt int) time.Duration {
	d := time.Duration(500*(attempt+1)*(attempt+1)) * time.Millisecond
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

func downloadChunk(ctx context.Context, client *http.Client, url string, f *os.File, from, to int64, counter *int64) error {
	const maxAttempts = 8
	curStart := from
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if curStart > to {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("User-Agent", DOWNLOAD_UA)
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("Range", "bytes="+strconv.FormatInt(curStart, 10)+"-"+strconv.FormatInt(to, 10))
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(backoff(attempt))
			continue
		}
		if resp.StatusCode != 206 {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
			resp.Body.Close()
			lastErr = fmt.Errorf("status %d (range not honored)", resp.StatusCode)
			if resp.StatusCode == 200 || isRetryableStatus(resp.StatusCode) {
				client.CloseIdleConnections()
				time.Sleep(backoff(attempt))
				continue
			}
			return lastErr
		}

		want := to - curStart + 1
		limited := io.LimitReader(resp.Body, want)
		offset := curStart
		buf := make([]byte, 256*1024)
		copyErr := func() error {
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				n, rerr := limited.Read(buf)
				if n > 0 {
					if _, werr := f.WriteAt(buf[:n], offset); werr != nil {
						return werr
					}
					offset += int64(n)
					atomic.AddInt64(counter, int64(n))
				}
				if rerr == io.EOF {
					return nil
				}
				if rerr != nil {
					return rerr
				}
			}
		}()
		resp.Body.Close()

		if copyErr == nil {
			return nil
		}
		if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
			return copyErr
		}
		lastErr = copyErr
		curStart = offset
		time.Sleep(backoff(attempt))
	}
	if lastErr != nil {
		return fmt.Errorf("max retries exceeded: %w", lastErr)
	}
	return errors.New("max retries exceeded")
}

func singleStreamDownload(ctx context.Context, client *http.Client, url, dst string, expected int64, ctype, cdisp string, maxBytes int64, prog ProgressFn) (*DownloadResult, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", DOWNLOAD_UA)
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if isCloudflareChallenge(resp.Header) {
			return nil, errors.New("cloudflare challenge blocked this download; paste the direct final .zip/.rar URL")
		}
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	if ctype == "" {
		ctype = resp.Header.Get("Content-Type")
	}
	if cdisp == "" {
		cdisp = resp.Header.Get("Content-Disposition")
	}
	if expected <= 0 {
		expected = resp.ContentLength
	}
	if maxBytes > 0 && expected > maxBytes {
		return nil, fmt.Errorf("file too large (%s, cap %s)", formatBytes(expected), formatBytes(maxBytes))
	}
	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var downloaded int64
	start := time.Now()
	stopProg := make(chan struct{})
	if prog != nil {
		go progLoop(stopProg, &downloaded, expected, start, prog)
	}
	buf := make([]byte, 256*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if maxBytes > 0 && downloaded+int64(n) > maxBytes {
				close(stopProg)
				os.Remove(dst)
				return nil, fmt.Errorf("file too large (cap %s)", formatBytes(maxBytes))
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				close(stopProg)
				return nil, werr
			}
			atomic.AddInt64(&downloaded, int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			close(stopProg)
			return nil, rerr
		}
	}
	close(stopProg)
	return &DownloadResult{
		Path:               dst,
		Bytes:              atomic.LoadInt64(&downloaded),
		Duration:           time.Since(start),
		Parallel:           1,
		RangeUsed:          false,
		ContentType:        ctype,
		ContentDisposition: cdisp,
		FileName:           filenameFromDisposition(cdisp),
		FinalURL:           resp.Request.URL.String(),
	}, nil
}

func isCloudflareChallenge(h http.Header) bool {
	return h.Get("cf-mitigated") == "challenge"
}

func filenameFromDisposition(v string) string {
	if v == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return filepath.Base(params["filename"])
}

func archiveExtFromName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".zip" || ext == ".rar" {
		return ext
	}
	return ""
}

func detectArchiveExt(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	head := make([]byte, 8)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	head = head[:n]
	if bytes.HasPrefix(head, []byte("PK\x03\x04")) ||
		bytes.HasPrefix(head, []byte("PK\x05\x06")) ||
		bytes.HasPrefix(head, []byte("PK\x07\x08")) {
		return ".zip", nil
	}
	if bytes.HasPrefix(head, []byte("Rar!\x1a\x07\x00")) ||
		bytes.HasPrefix(head, []byte("Rar!\x1a\x07\x01\x00")) {
		return ".rar", nil
	}
	return "", fmt.Errorf("downloaded response is not a zip/rar archive")
}

func formatBytes(n int64) string {
	const u = 1024.0
	if n < int64(u) {
		return fmt.Sprintf("%dB", n)
	}
	v := float64(n)
	units := []string{"KB", "MB", "GB", "TB"}
	i := 0
	v /= u
	for v >= u && i < len(units)-1 {
		v /= u
		i++
	}
	return fmt.Sprintf("%.2f%s", v, units[i])
}
