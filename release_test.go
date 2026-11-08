package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testDownloader ходит на httptest-сервер с его сертификатом и короткими паузами.
func testDownloader(srv *httptest.Server, backoff time.Duration) *downloader {
	return &downloader{
		client: &http.Client{
			Transport:     srv.Client().Transport,
			CheckRedirect: httpsOnlyRedirect,
			Timeout:       5 * time.Second,
		},
		attempts: 3,
		backoff:  backoff,
	}
}

// statusSequence отвечает кодами по очереди (последний повторяется) и считает запросы.
func statusSequence(t *testing.T, codes ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1)) - 1
		code := codes[min(i, len(codes)-1)]
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = io.WriteString(w, "payload")
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestDownloaderRetriesTransientErrors(t *testing.T) {
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusBadGateway} {
		srv, n := statusSequence(t, code, http.StatusOK)
		data, err := testDownloader(srv, time.Millisecond).get(context.Background(), srv.URL, 1024)
		if err != nil || string(data) != "payload" || n.Load() != 2 {
			t.Errorf("%d: data=%q err=%v requests=%d", code, data, err, n.Load())
		}
	}
}

func TestDownloaderDoesNotRetryClientErrors(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusUnauthorized} {
		srv, n := statusSequence(t, code)
		_, err := testDownloader(srv, time.Millisecond).get(context.Background(), srv.URL, 1024)
		var se *httpStatusError
		if !errors.As(err, &se) || se.Code != code || n.Load() != 1 {
			t.Errorf("%d: err=%v requests=%d", code, err, n.Load())
		}
	}
}

func TestDownloaderGivesUpAfterAttempts(t *testing.T) {
	srv, n := statusSequence(t, http.StatusInternalServerError)
	_, err := testDownloader(srv, time.Millisecond).get(context.Background(), srv.URL, 1024)
	if err == nil || n.Load() != 3 {
		t.Fatalf("err=%v requests=%d", err, n.Load())
	}
}

func TestDownloaderSizeLimit(t *testing.T) {
	big := strings.Repeat("x", 4096)
	withLength := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, big) // Content-Length известен заранее
	}))
	t.Cleanup(withLength.Close)
	chunked := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for range 4 {
			_, _ = io.WriteString(w, big[:1024])
			w.(http.Flusher).Flush() // без Content-Length — лимит ловится при чтении
		}
	}))
	t.Cleanup(chunked.Close)
	for _, srv := range []*httptest.Server{withLength, chunked} {
		_, err := testDownloader(srv, time.Millisecond).get(context.Background(), srv.URL, 1000)
		if !errors.Is(err, errTooLarge) {
			t.Errorf("%s: want errTooLarge, got %v", srv.URL, err)
		}
	}
}

func TestDownloaderRejectsDowngradeRedirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/evil.zip", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	_, err := testDownloader(srv, time.Millisecond).get(context.Background(), srv.URL, 1024)
	if err == nil || !strings.Contains(err.Error(), "небезопасный") {
		t.Fatalf("редирект на http должен отклоняться: %v", err)
	}
}

func TestDownloaderCancelDuringBackoff(t *testing.T) {
	srv, _ := statusSequence(t, http.StatusServiceUnavailable)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := testDownloader(srv, 10*time.Second).get(ctx, srv.URL, 1024)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 3*time.Second {
		t.Fatalf("err=%v за %v", err, time.Since(start))
	}
}

func TestDownloaderDownloadToFile(t *testing.T) {
	srv, _ := statusSequence(t, http.StatusOK)
	dst := filepath.Join(t.TempDir(), "f.zip")
	if err := testDownloader(srv, time.Millisecond).download(context.Background(), srv.URL, dst, 1024); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "payload" {
		t.Fatalf("%q", data)
	}
	// неудача не оставляет полуфайл
	bad, _ := statusSequence(t, http.StatusNotFound)
	dst2 := filepath.Join(t.TempDir(), "g.zip")
	if err := testDownloader(bad, time.Millisecond).download(context.Background(), bad.URL, dst2, 1024); err == nil {
		t.Fatal("ждал ошибку")
	}
	if _, err := os.Stat(dst2); !os.IsNotExist(err) {
		t.Fatal("после ошибки остался файл")
	}
}

// BenchmarkReleaseDownload сравнивает память при скачивании релиза: в буфер (как
// было — весь zip в памяти) и потоком в файл (как сейчас). Смотреть B/op.
func BenchmarkReleaseDownload(b *testing.B) {
	payload := make([]byte, 8<<20)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	b.Cleanup(srv.Close)
	d := testDownloader(srv, time.Millisecond)
	ctx := context.Background()
	b.Run("to-memory", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := d.get(ctx, srv.URL, maxReleaseZip); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("to-file", func(b *testing.B) {
		dst := filepath.Join(b.TempDir(), "r.zip")
		b.ReportAllocs()
		for b.Loop() {
			if err := d.download(ctx, srv.URL, dst, maxReleaseZip); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestBackoffDelayGrows(t *testing.T) {
	d := &downloader{backoff: time.Second}
	for attempt, lo := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second} {
		got := d.delay(attempt)
		if got < lo || got > lo+time.Second/2 {
			t.Errorf("attempt %d: %v вне [%v, %v]", attempt, got, lo, lo+time.Second/2)
		}
	}
}

func TestRetryable(t *testing.T) {
	cases := map[error]bool{
		&httpStatusError{Code: 500}:    true,
		&httpStatusError{Code: 429}:    true,
		&httpStatusError{Code: 404}:    false,
		errTooLarge:                    false,
		errors.New("connection reset"): true,
	}
	for err, want := range cases {
		if got := retryable(err); got != want {
			t.Errorf("retryable(%v) = %v", err, got)
		}
	}
}
