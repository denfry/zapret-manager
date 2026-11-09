// Package sitelist edits the user hostlists of a zapret-discord-youtube
// release: lists\list-general-user.txt (domains zapret must process) and
// lists\list-exclude-user.txt (domains zapret must leave alone).
//
// list-general-user.txt keeps the user's own lines as they are and one block
// owned by zapret-manager with the domains of catalog services:
//
//	# Never leave this file empty
//	domain.example.abc
//	example.com
//
//	# >>> zapret-manager: сервисы из каталога (меняются командой zapret-manager sites) >>>
//	# service: instagram
//	instagram.com
//	# <<< zapret-manager <<<
//
// The file is the only place where enabled services are recorded, so the
// choice survives release updates (the file is copied to the new release)
// and stays visible to people who edit lists by hand.
//
// Invariants (covered by tests and fuzzing):
//   - rendering is idempotent: Parse(Render(f)).Render() == Render(f);
//   - lines outside the block are never dropped (orphaned markers excepted);
//   - the rendered file always has at least one entry — Flowseal's
//     load_user_lists says "Never leave this file empty".
package sitelist

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"zapret-manager/internal/textutil"
)

// Block markers and the section header. Markers are matched after trimming
// spaces so that editors adding indentation do not break detection.
const (
	Begin         = "# >>> zapret-manager: сервисы из каталога (меняются командой zapret-manager sites) >>>"
	End           = "# <<< zapret-manager <<<"
	sectionPrefix = "# service:"
	// Placeholder is the entry service.bat writes into empty user lists.
	Placeholder = "domain.example.abc"
)

// Section is one catalog service inside the managed block.
type Section struct {
	ID      string
	Domains []string
}

// File is a parsed user hostlist.
type File struct {
	Lines    []string  // lines outside the block, verbatim, without trailing blanks
	Sections []Section // the managed block, in file order
}

// Parse never fails: whatever is not understood is kept as a user line.
// The block counts only when Begin is followed by End; an orphaned marker is
// dropped alone and the lines after it stay user lines.
func Parse(content string) File {
	text := strings.ReplaceAll(strings.TrimPrefix(content, textutil.BOM), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	begin, end := blockRange(lines)
	var f File
	var cur *Section
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if begin >= 0 && i > begin && i < end {
			switch {
			case t == "":
			case strings.HasPrefix(t, sectionPrefix):
				id := normalizeID(strings.TrimPrefix(t, sectionPrefix))
				if id == "" {
					cur = nil
					continue
				}
				cur = f.section(id)
			case strings.HasPrefix(t, "#"):
				// комментарии внутри блока — наши же, при записи блок пишется заново
			case cur == nil:
				f.Lines = append(f.Lines, t) // запись вне раздела: не теряем, выносим к своим
			default:
				if !containsFold(cur.Domains, t) {
					cur.Domains = append(cur.Domains, t)
				}
			}
			continue
		}
		if i == begin || i == end || isMarker(t) {
			continue
		}
		f.Lines = append(f.Lines, l)
	}
	f.trimLines()
	return f
}

// Render returns the file content with CRLF line endings.
func (f File) Render() string {
	f.trimLines()
	if !f.hasEntries() {
		f.Lines = append(f.Lines, Placeholder)
	}
	var b strings.Builder
	for _, l := range f.Lines {
		b.WriteString(l + "\r\n")
	}
	sections := f.liveSections()
	if len(sections) == 0 {
		return b.String()
	}
	b.WriteString("\r\n" + Begin + "\r\n")
	for _, s := range sections {
		b.WriteString(sectionPrefix + " " + s.ID + "\r\n")
		for _, d := range s.Domains {
			b.WriteString(d + "\r\n")
		}
	}
	b.WriteString(End + "\r\n")
	return b.String()
}

// UserDomains returns the entries outside the block, without comments and
// the service.bat placeholder.
func (f File) UserDomains() []string {
	var out []string
	for _, l := range f.Lines {
		t := strings.TrimSpace(l)
		if isEntry(t) && !strings.EqualFold(t, Placeholder) {
			out = append(out, t)
		}
	}
	return out
}

// AddDomain appends a domain to the user lines; false — it is already there.
func (f *File) AddDomain(d string) bool {
	if containsFold(f.UserDomains(), d) {
		return false
	}
	f.trimLines()
	f.Lines = append(f.Lines, d)
	return true
}

// RemoveDomain removes every user line equal to d; false — there was none.
func (f *File) RemoveDomain(d string) bool {
	n := len(f.Lines)
	f.Lines = slices.DeleteFunc(f.Lines, func(l string) bool { return strings.EqualFold(strings.TrimSpace(l), d) })
	return len(f.Lines) != n
}

// Services returns the IDs of the sections in the block.
func (f File) Services() []string {
	var out []string
	for _, s := range f.liveSections() {
		out = append(out, s.ID)
	}
	return out
}

// HasService reports whether the block has a section with this ID.
func (f File) HasService(id string) bool {
	return slices.Contains(f.Services(), normalizeID(id))
}

// SetService adds the section or replaces its domains; false — nothing changed.
// A service without domains is kept as a bare header: it still records the
// choice (such services are covered by the ipset, not by the hostlist).
func (f *File) SetService(id string, domains []string) bool {
	id = normalizeID(id)
	if id == "" {
		return false
	}
	// то же, что оставил бы Parse: иначе запись и повторное чтение разойдутся
	var clean []string
	for _, d := range domains {
		d = strings.TrimSpace(d)
		if isEntry(d) && !containsFold(clean, d) {
			clean = append(clean, d)
		}
	}
	existed := f.HasService(id)
	s := f.section(id)
	if existed && slices.Equal(s.Domains, clean) {
		return false
	}
	s.Domains = clean
	return true
}

// RemoveService deletes the section; false — there was none.
func (f *File) RemoveService(id string) bool {
	id = normalizeID(id)
	n := len(f.Sections)
	f.Sections = slices.DeleteFunc(f.Sections, func(s Section) bool { return s.ID == id })
	return len(f.Sections) != n
}

func (f *File) section(id string) *Section {
	for i := range f.Sections {
		if f.Sections[i].ID == id {
			return &f.Sections[i]
		}
	}
	f.Sections = append(f.Sections, Section{ID: id})
	return &f.Sections[len(f.Sections)-1]
}

// liveSections drops sections whose ID is empty (zero values never written).
func (f File) liveSections() []Section {
	var out []Section
	for _, s := range f.Sections {
		if s.ID != "" {
			out = append(out, s)
		}
	}
	return out
}

func (f *File) trimLines() {
	for len(f.Lines) > 0 && strings.TrimSpace(f.Lines[len(f.Lines)-1]) == "" {
		f.Lines = f.Lines[:len(f.Lines)-1]
	}
}

func (f File) hasEntries() bool {
	for _, l := range f.Lines {
		if isEntry(strings.TrimSpace(l)) {
			return true
		}
	}
	for _, s := range f.liveSections() {
		if len(s.Domains) > 0 {
			return true
		}
	}
	return false
}

func isEntry(t string) bool { return t != "" && !strings.HasPrefix(t, "#") }

func isMarker(t string) bool { return t == Begin || t == End }

// normalizeID keeps IDs to [a-z0-9-] so that a header always round-trips.
func normalizeID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 40 {
		return ""
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return ""
		}
	}
	return s
}

func blockRange(lines []string) (begin, end int) {
	begin = -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == Begin && begin < 0 {
			begin = i
		} else if t == End && begin >= 0 {
			return begin, i
		}
	}
	return -1, -1
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

// ---------- домены от пользователя ----------

// NormalizeDomain turns what a person types — "example.com", "https://www.example.com/page",
// "*.example.com:443" — into a hostlist entry: lower-case ASCII host without
// "www." (zapret matches subdomains anyway). IP addresses and non-ASCII names
// are rejected: hostlists match names only, and punycode needs no extra
// dependency when the person pastes it.
func NormalizeDomain(s string) (string, error) {
	in := strings.TrimSpace(s)
	host := strings.ToLower(in)
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("%q: не похоже на адрес сайта", in)
		}
		host = u.Hostname()
	} else {
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if h, port, err := net.SplitHostPort(host); err == nil {
			if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
				return "", fmt.Errorf("%q: неверный порт %q", in, port)
			}
			host = h
		}
	}
	host = strings.TrimSuffix(host, ".")
	host = strings.TrimPrefix(host, "*.")
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return "", fmt.Errorf("%q — это IP-адрес, а списки zapret работают с именами сайтов", in)
	}
	for strings.HasPrefix(host, "www.") && strings.Count(host, ".") >= 2 {
		host = strings.TrimPrefix(host, "www.")
	}
	if err := validateHost(host); err != nil {
		return "", fmt.Errorf("%q: %w", in, err)
	}
	return host, nil
}

var errNotASCII = errors.New("имя с кириллицей укажите в punycode (xn--…): пример.рф — это xn--e1afmkfd.xn--p1ai")

func validateHost(h string) error {
	if h == "" {
		return errors.New("пустое имя")
	}
	if len(h) > 253 {
		return errors.New("слишком длинное имя")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return errors.New("нужно имя с точкой, например example.com")
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 {
			return errors.New("пустая или слишком длинная часть имени")
		}
		if l[0] == '-' || l[len(l)-1] == '-' {
			return errors.New("часть имени не может начинаться или заканчиваться дефисом")
		}
		for i := range len(l) {
			c := l[i]
			switch {
			case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			case c >= 0x80:
				return errNotASCII
			default:
				return fmt.Errorf("недопустимый символ %q", c)
			}
		}
	}
	tld := labels[len(labels)-1]
	if strings.Trim(tld, "0123456789") == "" {
		return errors.New("домен верхнего уровня не может состоять из цифр")
	}
	return nil
}
