package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

type Target struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Group string `json:"group"` // discord | youtube | control | любая своя группа
}

// defaultTargets: Discord и YouTube — то, что проверяем; ya.ru и vk.com есть в
// list-exclude, zapret их не трогает — по ним понимаем, что пропал сам интернет.
func defaultTargets() []Target {
	return []Target{
		{"Discord", "https://discord.com/", "discord"},
		{"Discord API", "https://discord.com/api/v9/gateway", "discord"},
		{"Discord Gateway", "https://gateway.discord.gg/", "discord"},
		{"Discord CDN", "https://cdn.discordapp.com/", "discord"},
		{"Discord Media", "https://media.discordapp.net/", "discord"},
		{"Discord Updates", "https://updates.discord.com/", "discord"},
		{"YouTube", "https://www.youtube.com/", "youtube"},
		{"YouTube картинки", "https://i.ytimg.com/vi/dQw4w9WgXcQ/hqdefault.jpg", "youtube"},
		{"YouTube аватарки", "https://yt3.ggpht.com/", "youtube"},
		{"YouTube API", "https://youtubei.googleapis.com/", "youtube"},
		{"Яндекс (контроль)", "https://ya.ru/", "control"},
		{"VK (контроль)", "https://vk.com/", "control"},
	}
}

func (a *App) targets() []Target {
	if len(a.Cfg.Targets) > 0 {
		return a.Cfg.Targets
	}
	return defaultTargets()
}

const (
	checkTimeout = 8 * time.Second // на один адрес при обычной проверке
	sweepTimeout = 6 * time.Second // на один адрес при подборе
	// 48 КБ тела + сертификат — больше порога «заморозки» ТСПУ на 16-20 КБ, но мало трафика.
	maxBodyBytes = 48 * 1024
)

type CheckResult struct {
	Target
	OK      bool
	Kind    string // ok | dns | cert | connect | refused | reset | tls-timeout | tls | no-response | freeze | http | bad-url
	Detail  string
	Status  int
	Bytes   int64
	Elapsed time.Duration
}

func kindText(r CheckResult) string {
	switch r.Kind {
	case "ok":
		return "ok"
	case "dns":
		return "DNS не отвечает или подменён"
	case "cert":
		return "подменён сертификат (DNS провайдера / заглушка)"
	case "connect":
		return "нет соединения (IP недоступен)"
	case "refused":
		return "соединение отклонено"
	case "reset":
		return "соединение сброшено (DPI)"
	case "tls-timeout":
		return "TLS завис (DPI)"
	case "tls":
		return "ошибка TLS"
	case "no-response":
		return "нет ответа после запроса"
	case "freeze":
		return fmt.Sprintf("загрузка встала на %d КБ (DPI 16-20 КБ)", r.Bytes/1024)
	case "http":
		return "битый ответ"
	}
	return r.Kind
}

type GroupStat struct {
	OK    int
	Total int
}

func (g *GroupStat) String() string {
	if g == nil || g.Total == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", g.OK, g.Total)
}

type HealthReport struct {
	Time        time.Time
	Results     []CheckResult
	Groups      map[string]*GroupStat
	Score       int
	Max         int
	LatencyMs   int64
	NetworkDown bool
	DNSProblem  bool
	Healthy     bool
}

func (r *HealthReport) group(name string) *GroupStat {
	if g, ok := r.Groups[name]; ok {
		return g
	}
	return &GroupStat{}
}

// otherGroups — свои группы из config.json (не discord/youtube/control).
func (r *HealthReport) otherGroups() string {
	var parts []string
	for name, g := range r.Groups {
		if name == "discord" || name == "youtube" || name == "control" {
			continue
		}
		parts = append(parts, name+" "+g.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func (r *HealthReport) failures() []string {
	var out []string
	for _, res := range r.Results {
		if !res.OK && res.Group != "control" {
			out = append(out, res.Name+": "+kindText(res))
		}
	}
	return out
}

func (r *HealthReport) summary(strategy string) *CheckSummary {
	return &CheckSummary{
		Time:        r.Time,
		Strategy:    strategy,
		Discord:     r.group("discord").String(),
		YouTube:     r.group("youtube").String(),
		Other:       r.otherGroups(),
		Healthy:     r.Healthy,
		NetworkDown: r.NetworkDown,
		DNSProblem:  r.DNSProblem,
		Failures:    r.failures(),
	}
}

func (r *HealthReport) shortText() string {
	s := "Discord " + r.group("discord").String() + ", YouTube " + r.group("youtube").String()
	if o := r.otherGroups(); o != "" {
		s += ", " + o
	}
	return s
}

func runHealth(ctx context.Context, targets []Target, ratio float64, timeout time.Duration) *HealthReport {
	rep := &HealthReport{Time: time.Now(), Groups: map[string]*GroupStat{}}
	results := make([]CheckResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			results[i] = checkTarget(ctx, t, timeout, maxBodyBytes)
		}(i, t)
	}
	wg.Wait()
	rep.Results = results
	failed, dnsLike := 0, 0
	for _, res := range results {
		g := rep.Groups[res.Group]
		if g == nil {
			g = &GroupStat{}
			rep.Groups[res.Group] = g
		}
		g.Total++
		if res.OK {
			g.OK++
		}
		if res.Group == "control" {
			continue
		}
		rep.Max++
		if res.OK {
			rep.Score++
			rep.LatencyMs += res.Elapsed.Milliseconds()
		} else {
			failed++
			if res.Kind == "dns" || res.Kind == "cert" {
				dnsLike++
			}
		}
	}
	if c := rep.Groups["control"]; c != nil && c.Total > 0 && c.OK == 0 {
		rep.NetworkDown = true
	}
	rep.DNSProblem = failed > 0 && dnsLike*2 >= failed
	rep.Healthy = !rep.NetworkDown
	for name, g := range rep.Groups {
		if name == "control" || g.Total == 0 {
			continue
		}
		if float64(g.OK)/float64(g.Total) < ratio-1e-9 {
			rep.Healthy = false
		}
	}
	return rep
}

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"

// checkTarget делает настоящий запрос: TCP → TLS → GET → читает тело до maxBody.
// Фаза, на которой всё сломалось, говорит, что именно режет провайдер.
// Системный прокси не используем: проверяем прямой путь, через который идёт zapret.
func checkTarget(ctx context.Context, t Target, timeout time.Duration, maxBody int64) (res CheckResult) {
	res = CheckResult{Target: t}
	start := time.Now()
	defer func() { res.Elapsed = time.Since(start) }()

	u, err := url.Parse(t.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		res.Kind, res.Detail = "bad-url", t.URL
		return
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()

	connectTimeout := timeout / 2
	if connectTimeout > 4*time.Second {
		connectTimeout = 4 * time.Second
	}
	d := net.Dialer{Timeout: connectTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		res.Kind, res.Detail = classifyErr(err, "connect"), err.Error()
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	var rw net.Conn = conn
	if u.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: host, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			res.Kind, res.Detail = classifyErr(err, "tls"), err.Error()
			return
		}
		rw = tc
	}
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"User-Agent: " + userAgent + "\r\n" +
		"Accept: text/html,application/xhtml+xml,application/json,image/*,*/*;q=0.8\r\n" +
		"Accept-Language: ru-RU,ru;q=0.9,en;q=0.8\r\n" +
		"Accept-Encoding: identity\r\n" +
		"Connection: close\r\n\r\n"
	if _, err := io.WriteString(rw, req); err != nil {
		res.Kind, res.Detail = classifyErr(err, "no-response"), err.Error()
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(rw), nil)
	if err != nil {
		res.Kind, res.Detail = classifyErr(err, "no-response"), err.Error()
		return
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	res.Bytes = n
	if err != nil {
		kind := classifyErr(err, "freeze")
		if kind == "freeze" && n == 0 {
			kind = "no-response"
		}
		res.Kind, res.Detail = kind, err.Error()
		return
	}
	res.OK, res.Kind = true, "ok"
	return
}

func classifyErr(err error, phase string) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var certErr *tls.CertificateVerificationError
	var uaErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invErr x509.CertificateInvalidError
	if errors.As(err, &certErr) || errors.As(err, &uaErr) || errors.As(err, &hostErr) || errors.As(err, &invErr) {
		return "cert"
	}
	if errors.Is(err, windows.WSAECONNRESET) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, windows.WSAECONNABORTED) {
		return "reset"
	}
	if errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, syscall.ECONNREFUSED) {
		return "refused"
	}
	var ne net.Error
	timeout := errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
	switch phase {
	case "connect":
		return "connect"
	case "tls":
		if timeout {
			return "tls-timeout"
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "reset"
		}
		return "tls"
	case "no-response":
		if timeout {
			return "no-response"
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "reset"
		}
		return "http"
	case "freeze":
		if timeout {
			return "freeze"
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return "reset"
		}
		return "http"
	}
	return phase
}
