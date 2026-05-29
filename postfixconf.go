package main

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// PostfixConf holds the direction-relevant facts derived from a Postfix configuration
// directory: the trusted source networks (mynetworks) and the local/hosted domains
// (mydestination, mydomain/myhostname, relay_domains, virtual_*_domains). It lets the
// direction classifier tell "from us / to us" apart from internet transit when the IP
// heuristic alone is ambiguous — e.g. a content-filter gateway whose own backend (a
// public IP listed in mynetworks) submits OUTBOUND mail looks identical, by IP, to an
// external sender delivering INBOUND mail. Derived purely from the files; lookup tables
// backed by MySQL/LDAP/etc. are skipped (unreadable without the DB).
type PostfixConf struct {
	Hostname     string          // myhostname, for display on the Servers page
	LocalDomains map[string]bool // lowercased, no trailing dot
	Networks     []*net.IPNet
}

// inMyNetworks reports whether ip is inside the configured mynetworks (a trusted source).
func (pc *PostfixConf) inMyNetworks(ipStr string) bool {
	if pc == nil || ipStr == "" {
		return false
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, n := range pc.Networks {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// isLocalDomain reports whether the domain of addr (an envelope address or bare domain)
// is one of the configured local/hosted domains, or a subdomain of one.
func (pc *PostfixConf) isLocalDomain(addr string) bool {
	if pc == nil || addr == "" {
		return false
	}
	d := addr
	if i := strings.LastIndexByte(d, '@'); i >= 0 {
		d = d[i+1:]
	}
	d = strings.ToLower(strings.Trim(d, " <>"))
	if d == "" {
		return false
	}
	if pc.LocalDomains[d] {
		return true
	}
	for ld := range pc.LocalDomains {
		if strings.HasSuffix(d, "."+ld) {
			return true
		}
	}
	return false
}

// ConfPayload is the wire form of a PostfixConf (forwarder → receiver), since a
// net.IPNet doesn't round-trip through JSON. The CIDRs are already cap-filtered on the
// forwarder, so the receiver reconstructs them verbatim.
type ConfPayload struct {
	Hostname     string   `json:"hostname"`
	LocalDomains []string `json:"local_domains"`
	Networks     []string `json:"networks"`
}

func (pc *PostfixConf) payload() *ConfPayload {
	if pc == nil {
		return nil
	}
	doms := make([]string, 0, len(pc.LocalDomains))
	for d := range pc.LocalDomains {
		doms = append(doms, d)
	}
	sort.Strings(doms)
	nets := make([]string, 0, len(pc.Networks))
	for _, n := range pc.Networks {
		nets = append(nets, n.String())
	}
	return &ConfPayload{Hostname: pc.Hostname, LocalDomains: doms, Networks: nets}
}

func (p *ConfPayload) toConf() *PostfixConf {
	if p == nil {
		return nil
	}
	pc := &PostfixConf{Hostname: p.Hostname, LocalDomains: map[string]bool{}}
	for _, d := range p.LocalDomains {
		pc.LocalDomains[strings.ToLower(d)] = true
	}
	for _, c := range p.Networks {
		if _, n, err := net.ParseCIDR(c); err == nil {
			pc.Networks = append(pc.Networks, n)
		}
	}
	return pc
}

// signature is a stable string of the derived facts, so a watcher can swap only on change.
func (pc *PostfixConf) signature() string {
	if pc == nil {
		return ""
	}
	doms := make([]string, 0, len(pc.LocalDomains))
	for d := range pc.LocalDomains {
		doms = append(doms, d)
	}
	sort.Strings(doms)
	nets := make([]string, 0, len(pc.Networks))
	for _, n := range pc.Networks {
		nets = append(nets, n.String())
	}
	sort.Strings(nets)
	return strings.Join(doms, ",") + "|" + strings.Join(nets, ",")
}

var reMainCfVar = regexp.MustCompile(`\$\{?(\w+)\}?`)

// loadPostfixConf derives a PostfixConf from a /etc/postfix-style directory: parses
// main.cf, expands $variables, and follows file-backed lookup tables (hash:/cidr:/…)
// referenced by the relevant parameters. Database-backed tables are skipped.
func loadPostfixConf(dir string) (*PostfixConf, error) {
	params, err := parseMainCf(filepath.Join(dir, "main.cf"))
	if err != nil {
		return nil, err
	}
	pc := &PostfixConf{LocalDomains: map[string]bool{}}
	pc.Hostname = strings.ToLower(strings.TrimSpace(expandMainCf(params, params["myhostname"])))

	for _, key := range []string{"mydestination", "mydomain", "myhostname",
		"virtual_alias_domains", "virtual_mailbox_domains", "relay_domains"} {
		for _, tok := range splitFields(expandMainCf(params, params[key])) {
			pc.addDomainToken(tok, dir)
		}
	}
	for _, tok := range splitFields(expandMainCf(params, params["mynetworks"])) {
		pc.addNetworkToken(tok, dir)
	}
	return pc, nil
}

// parseMainCf reads main.cf into a name→value map, joining continuation lines (those
// starting with whitespace) and dropping comments.
func parseMainCf(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := map[string]string{}
	var key, val string
	flush := func() {
		if key != "" {
			m[key] = strings.TrimSpace(val)
		}
		key, val = "", ""
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' { // continuation of the previous value
			val += " " + trimmed
			continue
		}
		flush()
		if eq := strings.IndexByte(line, '='); eq >= 0 {
			key = strings.TrimSpace(line[:eq])
			val = strings.TrimSpace(line[eq+1:])
		}
	}
	flush()
	return m, sc.Err()
}

// expandMainCf resolves $name / ${name} references against the parsed params.
func expandMainCf(params map[string]string, val string) string {
	for i := 0; i < 10 && strings.Contains(val, "$"); i++ {
		val = reMainCfVar.ReplaceAllStringFunc(val, func(m string) string {
			return params[strings.Trim(m, "${}")]
		})
	}
	return val
}

// splitFields splits a parameter value on whitespace and commas.
func splitFields(val string) []string {
	return strings.FieldsFunc(val, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ','
	})
}

func (pc *PostfixConf) addDomainToken(tok, dir string) {
	if p := tableFilePath(tok, dir); p != "" {
		for _, k := range readMapKeys(p) {
			pc.addDomain(k)
		}
		return
	}
	if strings.ContainsAny(tok, ":/") { // an unreadable table spec (mysql/ldap/proxy/…)
		return
	}
	pc.addDomain(tok)
}

func (pc *PostfixConf) addDomain(d string) {
	if i := strings.LastIndexByte(d, '@'); i >= 0 {
		d = d[i+1:]
	}
	d = strings.ToLower(strings.Trim(d, " .<>"))
	if d == "" || d == "localhost" || strings.HasPrefix(d, "/") || !strings.Contains(d, ".") {
		return
	}
	pc.LocalDomains[d] = true
}

func (pc *PostfixConf) addNetworkToken(tok, dir string) {
	// A table spec (cidr:/path, hash:/path, …) — but not an IPv6 literal like [::1]/128.
	if i := strings.IndexByte(tok, ':'); i > 0 && !strings.HasPrefix(tok, "[") {
		if p := tableFilePath(tok, dir); p != "" {
			for _, k := range readMapKeys(p) {
				pc.addCIDR(k)
			}
			return
		}
		if !strings.Contains(tok, "::") { // a non-file table type we can't read; not IPv6
			return
		}
	}
	pc.addCIDR(tok)
}

func (pc *PostfixConf) addCIDR(tok string) {
	tok = strings.NewReplacer("[", "", "]", "").Replace(strings.TrimSpace(tok))
	if tok == "" {
		return
	}
	if !strings.Contains(tok, "/") {
		ip := net.ParseIP(tok)
		if ip == nil {
			return
		}
		if ip.To4() != nil {
			tok += "/32"
		} else {
			tok += "/128"
		}
	}
	_, n, err := net.ParseCIDR(tok)
	if err != nil {
		return
	}
	// Drop over-broad IPv4 entries: a public /8-/15 in mynetworks (e.g. a stray
	// 168.0.0.0/8) would wrongly trust external senders that happen to fall in it. No
	// real gateway trusts that many addresses. Private/loopback broad ranges are harmless
	// to drop here — the public-IP check already excludes those clients/relays — so only
	// SPECIFIC public ranges (the actual backends, /16 and narrower) are kept.
	if ones, bits := n.Mask.Size(); bits == 32 && ones < 16 {
		return
	}
	pc.Networks = append(pc.Networks, n)
}

// tableFilePath resolves a Postfix lookup spec to a readable file path, or "" if it is
// not a file-backed table (mysql/ldap/pgsql/proxy:other/…) or the file isn't present.
// The path is tried as-is (production: the dir IS the mounted /etc/postfix) and then by
// basename inside dir (local: a copied tree under another root).
func tableFilePath(spec, dir string) string {
	s := strings.TrimPrefix(spec, "proxy:")
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return ""
	}
	switch s[:i] {
	case "hash", "btree", "lmdb", "cdb", "dbm", "sdbm", "texthash", "cidr", "pcre", "regexp":
	default:
		return "" // mysql, ldap, pgsql, memcache, socketmap, static, fail, tcp, …
	}
	path := s[i+1:]
	if fileReadable(path) {
		return path
	}
	if alt := filepath.Join(dir, filepath.Base(path)); fileReadable(alt) {
		return alt
	}
	return ""
}

func fileReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// readMapKeys returns the first token (the key) of every non-comment line in a Postfix
// lookup file. Regex-table lines (keys beginning with "/") are skipped.
func readMapKeys(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var keys []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "/") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) > 0 {
			keys = append(keys, fields[0])
		}
	}
	return keys
}
