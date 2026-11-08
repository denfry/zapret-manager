package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// tlsServer поднимает HTTPS на 127.0.0.1 и возвращает пул с его сертификатом.
func tlsServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // «bad certificate» в тесте подмены — ожидаемо
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

// listenLocal открывает TCP-порт на 127.0.0.1.
func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// rawListener принимает соединения и отдаёт их handle (в своей горутине).
func rawListener(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln := listenLocal(t)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
	return ln.Addr().String()
}

func TestCheckTargetOK(t *testing.T) {
	srv, pool := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 100<<10)) // больше maxBody — читаем только 48 КБ
	})
	r := checkTarget(context.Background(), Target{Name: "ok", URL: srv.URL + "/", Group: "discord"}, 5*time.Second, pool)
	if !r.OK || r.Kind != "ok" || r.Status != http.StatusOK || r.Bytes != maxBodyBytes {
		t.Fatalf("%+v", r)
	}
}

// Любой HTTP-ответ — значит путь до сервера открыт (Discord API на / отдаёт 404).
func TestCheckTargetHTTPErrorStatusIsReachable(t *testing.T) {
	srv, pool := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) })
	r := checkTarget(context.Background(), Target{URL: srv.URL + "/api"}, 5*time.Second, pool)
	if !r.OK || r.Status != http.StatusNotFound {
		t.Fatalf("%+v", r)
	}
}

// Сертификат не от доверенного центра — признак подмены (заглушка провайдера).
func TestCheckTargetUntrustedCert(t *testing.T) {
	srv, _ := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {})
	r := checkTarget(context.Background(), Target{URL: srv.URL + "/"}, 5*time.Second, nil)
	if r.OK || r.Kind != "cert" {
		t.Fatalf("%+v", r)
	}
}

// ТСПУ «замораживает» TLS-соединение после 16–20 КБ: заголовки пришли, тело встало.
func TestCheckTargetFreeze(t *testing.T) {
	srv, pool := tlsServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write(make([]byte, 20<<10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	r := checkTarget(context.Background(), Target{URL: srv.URL + "/"}, 1500*time.Millisecond, pool)
	if r.OK || r.Kind != "freeze" || r.Bytes < 16<<10 {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(kindText(r), "20 КБ") {
		t.Fatalf("kindText: %q", kindText(r))
	}
}

// DPI рвёт соединение (RST) на ClientHello с запрещённым SNI.
func TestCheckTargetResetDuringHandshake(t *testing.T) {
	addr := rawListener(t, func(c net.Conn) {
		buf := make([]byte, 512)
		_, _ = c.Read(buf) // дождаться ClientHello
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // закрыть с RST
		}
		_ = c.Close()
	})
	r := checkTarget(context.Background(), Target{URL: "https://" + addr + "/"}, 5*time.Second, nil)
	if r.OK || r.Kind != "reset" {
		t.Fatalf("%+v", r)
	}
}

// DPI молча глотает ClientHello: TCP есть, TLS не завершается.
func TestCheckTargetTLSHang(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	addr := rawListener(t, func(c net.Conn) {
		defer c.Close() //nolint:errcheck // test
		_, _ = io.Copy(io.Discard, c)
		<-done
	})
	r := checkTarget(context.Background(), Target{URL: "https://" + addr + "/"}, time.Second, nil)
	if r.OK || r.Kind != "tls-timeout" {
		t.Fatalf("%+v", r)
	}
}

// Сервер принял запрос и молчит.
func TestCheckTargetNoResponse(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	addr := rawListener(t, func(c net.Conn) {
		defer c.Close() //nolint:errcheck // test
		_, _ = c.Read(make([]byte, 4096))
		<-done
	})
	r := checkTarget(context.Background(), Target{URL: "http://" + addr + "/"}, time.Second, nil)
	if r.OK || r.Kind != "no-response" {
		t.Fatalf("%+v", r)
	}
}

func TestCheckTargetConnectFails(t *testing.T) {
	ln := listenLocal(t)
	addr := ln.Addr().String()
	_ = ln.Close() // порт свободен — соединение отклонят
	r := checkTarget(context.Background(), Target{URL: "https://" + addr + "/"}, 6*time.Second, nil)
	if r.OK || (r.Kind != "refused" && r.Kind != "connect") {
		t.Fatalf("%+v", r)
	}
}

func TestCheckTargetBadURL(t *testing.T) {
	for _, u := range []string{"ftp://example.com/", "https:///nohost", "::bad"} {
		if r := checkTarget(context.Background(), Target{URL: u}, time.Second, nil); r.Kind != "bad-url" {
			t.Errorf("%q: %+v", u, r)
		}
	}
}

func TestCheckTargetHonoursCancel(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	addr := rawListener(t, func(c net.Conn) { <-done; _ = c.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	r := checkTarget(ctx, Target{URL: "https://" + addr + "/"}, 10*time.Second, nil)
	if r.OK || time.Since(start) > 3*time.Second {
		t.Fatalf("отмена не сработала за %v: %+v", time.Since(start), r)
	}
}

// Все адреса проверяются параллельно, порядок результатов совпадает с порядком целей.
func TestRunHealthParallelKeepsOrder(t *testing.T) {
	srv, pool := tlsServer(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "ok")
	})
	var targets []Target
	for i := range 20 {
		targets = append(targets, Target{Name: fmt.Sprint("t", i), URL: srv.URL + "/", Group: "discord"})
	}
	start := time.Now()
	rep := runHealthWith(context.Background(), targets, 0.75, 5*time.Second, pool)
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("проверки идут не параллельно: %v", el)
	}
	for i, r := range rep.Results {
		if r.Name != targets[i].Name || !r.OK {
			t.Fatalf("результат %d: %+v", i, r)
		}
	}
	if !rep.Healthy || rep.Score != 20 || rep.Max != 20 {
		t.Fatalf("%+v", rep)
	}
}

func res(group, kind string) CheckResult {
	return CheckResult{Target: Target{Name: group + "-" + kind, Group: group}, OK: kind == "ok", Kind: kind, Elapsed: 100 * time.Millisecond}
}

func TestSummarize(t *testing.T) {
	cases := []struct {
		name               string
		results            []CheckResult
		healthy, down, dns bool
		score, max         int
	}{
		{"всё открывается",
			[]CheckResult{res("discord", "ok"), res("youtube", "ok"), res("control", "ok")},
			true, false, false, 2, 2},
		{"контрольные не открываются — нет интернета",
			[]CheckResult{res("discord", "reset"), res("control", "connect"), res("control", "connect")},
			false, true, false, 0, 1},
		{"один контрольный жив — интернет есть",
			[]CheckResult{res("discord", "reset"), res("control", "ok"), res("control", "connect")},
			false, false, false, 0, 1},
		{"3/4 при пороге 0.75 — здоров",
			[]CheckResult{res("discord", "ok"), res("discord", "ok"), res("discord", "ok"), res("discord", "freeze")},
			true, false, false, 3, 4},
		{"2/4 — нет",
			[]CheckResult{res("discord", "ok"), res("discord", "ok"), res("discord", "reset"), res("discord", "freeze")},
			false, false, false, 2, 4},
		{"неудачи из-за DNS и сертификата — проблема DNS",
			[]CheckResult{res("discord", "dns"), res("youtube", "cert"), res("youtube", "reset"), res("control", "ok")},
			false, false, true, 0, 3},
		{"своя группа тоже должна проходить порог",
			[]CheckResult{res("discord", "ok"), res("games", "reset")},
			false, false, false, 1, 2},
	}
	for _, c := range cases {
		rep := summarize(c.results, 0.75, nil)
		if rep.Healthy != c.healthy || rep.NetworkDown != c.down || rep.DNSProblem != c.dns || rep.Score != c.score || rep.Max != c.max {
			t.Errorf("%s: healthy=%v down=%v dns=%v score=%d/%d", c.name, rep.Healthy, rep.NetworkDown, rep.DNSProblem, rep.Score, rep.Max)
		}
	}
}

func TestReportTexts(t *testing.T) {
	rep := summarize([]CheckResult{res("discord", "ok"), res("youtube", "reset"), res("games", "ok"), res("control", "connect")}, 0.75, nil)
	if s := rep.shortText(); s != "Discord 1/1, YouTube 0/1, games 1/1" {
		t.Fatalf("shortText: %q", s)
	}
	f := rep.failures()
	if len(f) != 1 || !strings.HasPrefix(f[0], "youtube-reset: ") {
		t.Fatalf("failures (без control): %v", f)
	}
	sum := rep.summary("ALT")
	if sum.Strategy != "ALT" || sum.Discord != "1/1" || sum.YouTube != "0/1" || sum.Other != "games 1/1" {
		t.Fatalf("%+v", sum)
	}
}

func TestClassifyErr(t *testing.T) {
	reset := &net.OpError{Op: "read", Err: os.NewSyscallError("wsarecv", windows.WSAECONNRESET)}
	refused := &net.OpError{Op: "dial", Err: os.NewSyscallError("connectex", windows.WSAECONNREFUSED)}
	timeout := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}
	cases := []struct {
		err   error
		phase string
		want  string
	}{
		{&net.DNSError{Err: "no such host", Name: "x"}, "connect", "dns"},
		{x509.UnknownAuthorityError{}, "tls", "cert"},
		{x509.HostnameError{Certificate: &x509.Certificate{}, Host: "discord.com"}, "tls", "cert"},
		{&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "tls", "cert"},
		{reset, "tls", "reset"},
		{refused, "connect", "refused"},
		{syscall.ECONNRESET, "no-response", "reset"},
		{timeout, "tls", "tls-timeout"},
		{io.EOF, "tls", "reset"},
		{errors.New("other"), "tls", "tls"},
		{timeout, "connect", "connect"},
		{timeout, "no-response", "no-response"},
		{io.ErrUnexpectedEOF, "no-response", "reset"},
		{errors.New("malformed"), "no-response", "http"},
		{timeout, "freeze", "freeze"},
		{io.ErrUnexpectedEOF, "freeze", "reset"},
		{context.DeadlineExceeded, "freeze", "freeze"},
	}
	for _, c := range cases {
		if got := classifyErr(c.err, c.phase); got != c.want {
			t.Errorf("classifyErr(%v, %s) = %s, want %s", c.err, c.phase, got, c.want)
		}
	}
}

// Группа, которую не открывает ни одна стратегия, видна в отчёте, но не делает его
// «нездоровым» и не превращает чужие сбои в «проблему DNS».
func TestSummarizeSkip(t *testing.T) {
	results := []CheckResult{
		res("discord", "ok"), res("youtube", "ok"), res("control", "ok"),
		res("instagram", "cert"), res("instagram", "dns"), res("x", "ok"),
	}
	if rep := summarize(results, 0.75, nil); rep.Healthy || !rep.DNSProblem {
		t.Fatalf("без пропуска: healthy=%v dns=%v", rep.Healthy, rep.DNSProblem)
	}
	rep := summarize(results, 0.75, map[string]bool{"instagram": true})
	if !rep.Healthy || rep.DNSProblem || rep.Score != 3 || rep.Max != 5 {
		t.Fatalf("с пропуском: %+v", rep)
	}
	if f := rep.failures(); len(f) != 0 {
		t.Fatalf("пропущенные группы — не неисправности: %v", f)
	}
	if o := rep.otherGroups(); o != "Instagram 0/2 (zapret не помогает), X (Twitter) 1/1" {
		t.Fatalf("otherGroups: %q", o)
	}
	sum := rep.summary("ALT")
	if !slices.Equal(sum.Skipped, []string{"instagram"}) || sum.Groups["x"] != (GroupStat{1, 1}) || len(sum.Groups) != 2 {
		t.Fatalf("%+v", sum)
	}
	// Discord пропустить нельзя: skippedGroups его не отдаёт, но и здесь сбой Discord — сбой
	if rep := summarize([]CheckResult{res("discord", "reset"), res("instagram", "reset")}, 0.75, map[string]bool{"instagram": true}); rep.Healthy {
		t.Fatal("Discord не открылся — нездоров")
	}
}
