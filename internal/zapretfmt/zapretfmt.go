// Package zapretfmt parses and validates the small file formats used by a
// zapret-discord-youtube release and its service.bat: utils\game_filter.enabled,
// lists\ipset-all.txt modes and the domain/IP lists downloaded from upstream.
//
// GameFilter values are substituted into the winws command line of a
// LocalSystem service, so they are validated to contain only digits, commas
// and dashes.
package zapretfmt

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"zapret-manager/internal/textutil"
)

// ---------- game filter ----------

// Game filter modes as written by service.bat.
const (
	ModeDisabled = "disabled"
	ModeAll      = "all"
	ModeTCP      = "tcp"
	ModeUDP      = "udp"
)

// DefaultPortRange is used when the file has no (valid) range.
const DefaultPortRange = "1024-65535"

// emptyPort is the placeholder port service.bat uses for a disabled filter.
const emptyPort = "12"

// GameFilter is the parsed utils\game_filter.enabled.
type GameFilter struct {
	Mode     string // disabled | all | tcp | udp
	TCPRange string
	UDPRange string
}

// DisabledGameFilter is the value used when the file is absent.
func DisabledGameFilter() GameFilter {
	return GameFilter{Mode: ModeDisabled, TCPRange: DefaultPortRange, UDPRange: DefaultPortRange}
}

// ParseGameFilter understands both the current "mode=…/tcp=…/udp=…" format
// and the legacy single-word files ("all", "tcp", "udp"). Invalid ranges fall
// back to DefaultPortRange, unknown modes to disabled.
func ParseGameFilter(content string) GameFilter {
	gf := DisabledGameFilter()
	var tcpCand, udpCand string
	for line := range strings.SplitSeq(strings.ReplaceAll(content, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		switch k {
		case "mode":
			gf.Mode = strings.ToLower(v)
		case ModeAll:
			gf.Mode = ModeAll
		case ModeUDP:
			if v == "" {
				gf.Mode = ModeUDP
			} else {
				udpCand = v
			}
		case ModeTCP:
			if v == "" {
				gf.Mode = ModeTCP
			} else {
				tcpCand = v
			}
		}
	}
	if r, ok := ValidatePortRanges(tcpCand); ok {
		gf.TCPRange = r
	}
	if r, ok := ValidatePortRanges(udpCand); ok {
		gf.UDPRange = r
	}
	switch gf.Mode {
	case ModeAll, ModeTCP, ModeUDP:
	default:
		gf.Mode = ModeDisabled
	}
	return gf
}

// Vars returns %GameFilter%, %GameFilterTCP% and %GameFilterUDP% as
// service.bat sets them ("12" stands for "no port").
func (g GameFilter) Vars() (anyProto, tcp, udp string) {
	switch g.Mode {
	case ModeAll:
		return g.TCPRange, g.TCPRange, g.UDPRange
	case ModeTCP:
		return g.TCPRange, g.TCPRange, emptyPort
	case ModeUDP:
		return g.UDPRange, emptyPort, g.UDPRange
	}
	return emptyPort, emptyPort, emptyPort
}

func (g GameFilter) String() string {
	switch g.Mode {
	case ModeAll:
		return fmt.Sprintf("TCP %s и UDP %s", g.TCPRange, g.UDPRange)
	case ModeTCP:
		return "TCP " + g.TCPRange
	case ModeUDP:
		return "UDP " + g.UDPRange
	}
	return "выключен"
}

var portItemRe = regexp.MustCompile(`^[1-9][0-9]*(-[1-9][0-9]*)?$`)

// ValidatePortRanges accepts "80", "1024-65535", "443,1024-1934,1936-65535"
// (spaces ignored) and returns the normalized string. Ports are 1..65535 and
// every range is ascending.
func ValidatePortRanges(s string) (string, bool) {
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		return "", false
	}
	for item := range strings.SplitSeq(s, ",") {
		if !portItemRe.MatchString(item) {
			return "", false
		}
		lo, hi, found := strings.Cut(item, "-")
		if !found {
			hi = lo
		}
		if len(lo) > 5 || len(hi) > 5 {
			return "", false
		}
		a, errA := strconv.Atoi(lo)
		b, errB := strconv.Atoi(hi)
		if errA != nil || errB != nil || a > 65535 || b > 65535 || a > b {
			return "", false
		}
	}
	return s, true
}

// ---------- ipset ----------

// IpsetNoneMarker is the placeholder service.bat writes for the "none" mode.
const IpsetNoneMarker = "203.0.113.113/32"

// ipset-all.txt modes as shown by service.bat.
const (
	IpsetAny    = "any"
	IpsetNone   = "none"
	IpsetLoaded = "loaded"
)

// IpsetModeOf classifies ipset-all.txt content like service.bat: empty file —
// any, the placeholder — none, anything else — loaded.
func IpsetModeOf(content []byte) string {
	text := strings.TrimSpace(string(textutil.StripBOM(content)))
	if text == "" {
		return IpsetAny
	}
	if strings.Contains(text, IpsetNoneMarker) {
		return IpsetNone
	}
	return IpsetLoaded
}

// CountEntries counts non-empty, non-comment lines.
func CountEntries(content []byte) int {
	n := 0
	for l := range strings.SplitSeq(string(content), "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			n++
		}
	}
	return n
}

// MinIpsetEntries guards against an error page or a truncated download
// replacing the real ipset (upstream has tens of thousands of lines).
const MinIpsetEntries = 1000

// ValidateIpset accepts a downloaded ipset when it has at least
// MinIpsetEntries entries and ≥95% of them are IPs or CIDRs.
func ValidateIpset(lines []string) error {
	total, good := 0, 0
	for _, l := range lines {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		total++
		if isIPOrCIDR(l) {
			good++
		}
	}
	if total < MinIpsetEntries {
		return fmt.Errorf("в ipset всего %d строк — похоже на ошибку", total)
	}
	if good*100 < total*95 {
		return fmt.Errorf("в ipset %d из %d строк не похожи на IP/подсеть", total-good, total)
	}
	return nil
}

var domainLineRe = regexp.MustCompile(`^\^?[A-Za-z0-9*_.\-]+$`)

// ValidateList checks a downloaded list: ipset*.txt must contain only IPs or
// CIDRs, other lists only domain patterns; empty lists are rejected.
func ValidateList(name string, lines []string) error {
	n := 0
	for _, l := range lines {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		n++
		if strings.HasPrefix(name, "ipset") {
			if !isIPOrCIDR(l) {
				return fmt.Errorf("строка %q не IP", l)
			}
			continue
		}
		if !domainLineRe.MatchString(l) {
			return fmt.Errorf("строка %q не похожа на домен", l)
		}
	}
	if n == 0 {
		return errors.New("пустой список")
	}
	return nil
}

func isIPOrCIDR(s string) bool {
	if _, _, err := net.ParseCIDR(s); err == nil {
		return true
	}
	return net.ParseIP(s) != nil
}
