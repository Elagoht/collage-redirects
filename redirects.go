// Package redirects is a collage plugin for redirects kept in a file rather than in
// code: what a site migration leaves behind, hundreds of old addresses and where
// each one went.
//
//	app, err := collage.New(&collage.Config{
//		Plugins: []collage.Plugin{redirects.New(redirects.Options{FS: siteFS})},
//	})
//
// redirects.txt holds one rule a line, the old address, the new one, and a status:
//
//	# the old blog
//	/blog/*          /posts/:splat
//	/about-us        /about          301
//	/summer-sale     /sale           302
//	/old-product     -               410
//
// The rules are served by middleware, before routing, and the query string a reader
// arrived with is carried over. A file with a malformed line, a rule no request can
// reach, or rules that send a reader round in a circle stops the application from
// starting, with the line that is wrong.
//
// A static host cannot run the middleware, so the plugin is a collage
// RedirectSource: a static build asks it for its rules, in the router's own
// pattern syntax, and hands them to whichever plugin writes a host's
// configuration (elagoht/deploy). A prefix, "/blog/*", becomes the path itself,
// "/blog", and a catch-all under it, "/blog/{rest...}", with ":splat" in the
// destination spelled "{rest}".
package redirects

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/Elagoht/collage/pkg/collage"
)

// Name is the plugin's name, and the key its configuration is found under.
const Name = "elagoht/redirects"

// Options configures the plugin.
type Options struct {
	// FS holds the redirects file. Set, File is read from it — a missing file is
	// then an error. Unset, File is read from the working directory when it is
	// set, and no file is read when it is not.
	FS fs.FS `json:"-"`
	// File is the redirects file's name. Default "redirects.txt".
	File string `json:"file"`
	// Rules are redirects given in Go or configuration, checked after the file's.
	Rules []Rule `json:"rules"`
}

// Rule is one redirect.
type Rule struct {
	// From is the old path: "/about-us", or a prefix, "/blog/*", matching
	// "/blog", "/blog/" and everything under it.
	From string `json:"from"`
	// To is where it went: a path on the site or an absolute URL. For a prefix,
	// ":splat" in it is what the "*" matched. "-" for a rule that is gone.
	To string `json:"to"`
	// Status is 301, the default, 302, 307, 308, or 410 for a page that is gone
	// and has no replacement.
	Status int `json:"status"`
}

// rule is a Rule checked and ready to match.
type rule struct {
	Rule
	// base is From without its "/*", escaped as a request path is.
	base   string
	prefix bool
	// source names where the rule was written, for an error: "redirects.txt:12".
	source string
}

// Plugin serves the redirects.
type Plugin struct {
	opts  Options
	rules []rule
	// host answers a gone page with the site's own not-found page.
	host collage.Host
}

var (
	_ collage.Plugin         = (*Plugin)(nil)
	_ collage.RedirectSource = (*Plugin)(nil)
)

// New returns a plugin with opts as its starting point, which the application's
// own configuration is then decoded over.
func New(opts Options) *Plugin { return &Plugin{opts: opts} }

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.2.0" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

// Init reads the rules, refuses any that are wrong, warns of any that hide a page,
// and serves them.
func (p *Plugin) Init(ctx context.Context, host collage.Host) error {
	cfg, err := collage.PluginConfig(host, p.opts)
	if err != nil {
		return err
	}
	p.opts = cfg
	rules, err := p.load()
	if err != nil {
		return err
	}
	if err := check(rules); err != nil {
		return err
	}
	p.rules = rules
	p.host = host
	p.warnShadowedPages(ctx, host)
	p.warnUncarried(host)
	if len(p.rules) == 0 {
		return nil
	}
	return host.Use(p.middleware)
}

// load reads the file, when there is one, and the rules given in Go.
func (p *Plugin) load() ([]rule, error) {
	var rules []rule
	file := p.opts.File
	if p.opts.FS != nil || file != "" {
		if file == "" {
			file = "redirects.txt"
		}
		var body []byte
		var err error
		if p.opts.FS != nil {
			body, err = fs.ReadFile(p.opts.FS, file)
		} else {
			body, err = os.ReadFile(file)
		}
		if err != nil {
			return nil, fmt.Errorf("redirects: %w", err)
		}
		if rules, err = parse(file, body); err != nil {
			return nil, err
		}
	}
	for i, r := range p.opts.Rules {
		checked, err := newRule(r, fmt.Sprintf("Rules[%d]", i))
		if err != nil {
			return nil, err
		}
		rules = append(rules, checked)
	}
	return rules, nil
}

// parse reads a redirects file, name being what an error calls it. Each line is
// "FROM TO [STATUS]"; blank lines and everything after a "#" that begins a field
// are ignored.
func parse(name string, body []byte) ([]rule, error) {
	var rules []rule
	for n, line := range strings.Split(string(body), "\n") {
		source := fmt.Sprintf("%s:%d", name, n+1)
		fields := strings.Fields(line)
		for i, f := range fields {
			if strings.HasPrefix(f, "#") {
				fields = fields[:i]
				break
			}
		}
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 2 || len(fields) > 3 {
			return nil, fmt.Errorf("redirects: %s: want FROM TO [STATUS], got %q", source, strings.TrimSpace(line))
		}
		r := Rule{From: fields[0], To: fields[1]}
		if len(fields) == 3 {
			status, err := strconv.Atoi(fields[2])
			if err != nil {
				return nil, fmt.Errorf("redirects: %s: status %q is not a number", source, fields[2])
			}
			r.Status = status
		}
		if r.To == "-" && r.Status != http.StatusGone {
			return nil, fmt.Errorf("redirects: %s: \"-\" is a page that is gone, which is status 410", source)
		}
		checked, err := newRule(r, source)
		if err != nil {
			return nil, err
		}
		rules = append(rules, checked)
	}
	return rules, nil
}

// ErrInvalidRule wraps every refusal of a rule.
var ErrInvalidRule = errors.New("redirects: invalid rule")

func newRule(r Rule, source string) (rule, error) {
	bad := func(format string, args ...any) (rule, error) { // any: fmt's own variadic parameter
		return rule{}, fmt.Errorf("%w: %s: %s", ErrInvalidRule, source, fmt.Sprintf(format, args...))
	}
	if r.Status == 0 {
		r.Status = http.StatusMovedPermanently
	}
	switch r.Status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusGone:
	default:
		return bad("status %d is none of 301, 302, 307, 308, 410", r.Status)
	}
	if !strings.HasPrefix(r.From, "/") || strings.HasPrefix(r.From, "//") {
		return bad("from %q must be a path beginning with one /", r.From)
	}
	for _, field := range []struct{ name, value string }{{"from", r.From}, {"to", r.To}} {
		if controlCharacter(field.value) {
			return bad("%s %q holds a control character", field.name, field.value)
		}
	}
	if strings.ContainsAny(r.From, "?# \t") {
		return bad("from %q must be a path alone: a query or fragment cannot be matched", r.From)
	}
	out := rule{Rule: r, source: source}
	base := r.From
	if strings.HasSuffix(base, "/*") {
		out.prefix = true
		base = strings.TrimSuffix(base, "/*")
	}
	if strings.Contains(base, "*") {
		return bad("from %q: * may only end a path, as /*", r.From)
	}
	if base != "/" && base != "" {
		base = strings.TrimSuffix(base, "/")
	}
	out.base = (&url.URL{Path: base}).EscapedPath()

	if r.Status == http.StatusGone {
		if r.To != "" && r.To != "-" {
			return bad("a 410 is gone and goes nowhere: to must be \"-\", got %q", r.To)
		}
		out.To = "-"
		return out, nil
	}
	switch {
	case r.To == "" || r.To == "-":
		return bad("to is required for status %d", r.Status)
	case strings.HasPrefix(r.To, "/") && !strings.HasPrefix(r.To, "//"):
	default:
		u, err := url.Parse(r.To)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return bad("to %q must be a path beginning with one / or an http(s) URL", r.To)
		}
	}
	if strings.ContainsAny(r.To, " \t") {
		return bad("to %q holds a space", r.To)
	}
	if strings.Contains(r.To, ":splat") && !out.prefix {
		return bad("to %q uses :splat, which only a from ending in /* has", r.To)
	}
	return out, nil
}

// match returns the rule for an escaped request path and what its "*" matched.
// The first rule matching wins, as it does on Netlify and Cloudflare Pages.
func match(rules []rule, path string) (*rule, string, bool) {
	trimmed := path
	if trimmed != "/" {
		trimmed = strings.TrimSuffix(trimmed, "/")
	}
	for i := range rules {
		r := &rules[i]
		if !r.prefix {
			if trimmed == r.base {
				return r, "", true
			}
			continue
		}
		if trimmed == r.base || (r.base == "" && trimmed == "/") {
			return r, "", true
		}
		if strings.HasPrefix(path, r.base+"/") {
			return r, path[len(r.base)+1:], true
		}
	}
	return nil, "", false
}

// target is where r sends a request whose "*" matched splat.
func (r *rule) target(splat string) string {
	to := strings.ReplaceAll(r.To, ":splat", splat)
	// A splat beginning with "/" — "/blog//evil.example" — must not turn a path
	// into a protocol-relative URL on another host.
	for strings.HasPrefix(to, "//") {
		to = to[1:]
	}
	return to
}

// check refuses a rule no request reaches, a From given twice, and rules that send
// a reader round in a circle — a redirect a browser gives up on after twenty hops.
func check(rules []rule) error {
	for i := range rules {
		probe := rules[i].base
		if rules[i].prefix {
			probe += "/loop-probe"
		}
		if probe == "" {
			probe = "/"
		}
		if first, _, _ := match(rules, probe); first != &rules[i] {
			return fmt.Errorf("%w: %s: %s is never reached: %s (%s) matches it first", ErrInvalidRule, rules[i].source, rules[i].From, first.From, first.source)
		}
	}
	for i := range rules {
		start := rules[i].base
		if rules[i].prefix {
			start += "/loop-probe"
		}
		if start == "" {
			start = "/"
		}
		seen := map[string]bool{start: true}
		path := start
		for hop := 0; ; hop++ {
			r, splat, ok := match(rules, path)
			if !ok || r.To == "-" {
				break
			}
			next := r.target(splat)
			if !strings.HasPrefix(next, "/") {
				break // another host: where it goes from there is not ours to know
			}
			if cut := strings.IndexAny(next, "?#"); cut >= 0 {
				next = next[:cut]
			}
			// A path seen before is a circle; a chain longer than there are rules
			// is a splat feeding itself, /x/* to /x/y/:splat, which never repeats a
			// path and never ends.
			if seen[next] || hop > len(rules) {
				return fmt.Errorf("%w: %s: %s redirects in a circle, through %s (%s)", ErrInvalidRule, rules[i].source, rules[i].From, r.From, r.source)
			}
			seen[next] = true
			path = next
		}
	}
	return nil
}

// warnShadowedPages logs a rule that hides a registered page: the middleware runs
// before routing, so the page can no longer be reached at that URL. That may be the
// point — a page replaced by a redirect before its code was removed — so it is a
// warning, not a refusal. Only the URLs Host.PageURLs knows are checked: a pattern
// without WithStaticParams has none.
func (p *Plugin) warnShadowedPages(ctx context.Context, host collage.Host) {
	for _, page := range host.Pages() {
		urls, err := host.PageURLs(ctx, page.Name)
		if err != nil {
			continue
		}
		for _, u := range urls {
			if r, _, ok := match(p.rules, (&url.URL{Path: u.Path}).EscapedPath()); ok {
				host.Logger().Warn("redirects: a rule hides a page", "rule", r.From, "at", r.source, "page", page.Name, "path", u.Path)
			}
		}
	}
}

func (p *Plugin) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rule, splat, ok := match(p.rules, r.URL.EscapedPath())
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		if rule.Status == http.StatusGone {
			// The site's own not-found page, with the status saying the page is not
			// coming back: a reader sees the site, a crawler drops the URL.
			p.host.ServeStatus(w, r, http.StatusGone)
			return
		}
		http.Redirect(w, r, withQuery(rule.target(splat), r.URL.RawQuery), rule.Status)
	})
}

// withQuery adds the request's query to target, after any query of its own and
// before its fragment.
func withQuery(target, query string) string {
	if query == "" {
		return target
	}
	fragment := ""
	if i := strings.IndexByte(target, '#'); i >= 0 {
		target, fragment = target[:i], target[i:]
	}
	if strings.Contains(target, "?") {
		return target + "&" + query + fragment
	}
	return target + "?" + query + fragment
}

// controlCharacter reports whether s holds a character that would split a line
// of a host's file or a header: the set collage refuses in a static build's
// redirects.
func controlCharacter(s string) bool {
	for _, c := range s {
		if unicode.IsControl(c) || c == '\u2028' || c == '\u2029' {
			return true
		}
	}
	return false
}

// Redirects returns the rules for a static build, in collage's pattern syntax and
// in the order the middleware tries them. A prefix "/x/*" is the path "/x",
// unless an earlier rule takes it, and the catch-all "/x/{rest...}", whose
// ":splat" is "{rest}"; collage's catch-all needs at least one segment, the
// plugin's prefix does not. A 410 has no destination. A rule the router's
// grammar cannot say is left out; Init warned of it.
func (p *Plugin) Redirects() []collage.BuiltRedirect {
	var out []collage.BuiltRedirect
	for i := range p.rules {
		r := &p.rules[i]
		if uncarried(r) != "" {
			continue
		}
		to := r.To
		if r.Status == http.StatusGone {
			to = ""
		}
		if !r.prefix {
			out = append(out, collage.BuiltRedirect{From: r.From, To: to, Status: r.Status})
			continue
		}
		base := strings.TrimSuffix(r.From, "/*")
		probe := r.base
		if probe == "" {
			probe = "/"
		}
		if first, _, _ := match(p.rules, probe); first == r {
			literal := to
			if literal != "" {
				literal = r.target("")
			}
			from := base
			if from == "" {
				from = "/"
			}
			out = append(out, collage.BuiltRedirect{From: from, To: literal, Status: r.Status})
		}
		out = append(out, collage.BuiltRedirect{
			From:   base + "/{rest...}",
			To:     strings.ReplaceAll(to, ":splat", "{rest}"),
			Status: r.Status,
		})
	}
	return out
}

// uncarried returns why collage's router cannot say r, or "": a "{" or "}",
// which the router reads as a placeholder and the plugin as text; an empty
// segment, which no pattern has; or ":splat" in an absolute destination's
// scheme://authority, which would let a request choose the host.
func uncarried(r *rule) string {
	switch {
	case strings.ContainsAny(r.From, "{}"):
		return "from holds a { or }, which collage reads as a placeholder"
	case r.Status != http.StatusGone && strings.ContainsAny(r.To, "{}"):
		return "to holds a { or }, which collage reads as a placeholder"
	case strings.Contains(r.From, "//"):
		return "from has an empty segment, which no collage pattern has"
	}
	if at := strings.Index(r.To, "://"); r.prefix && at >= 0 {
		authority := r.To[at+3:]
		if end := strings.IndexAny(authority, "/?#"); end >= 0 {
			authority = authority[:end]
		}
		if strings.Contains(authority, ":splat") {
			return "to has :splat in its host, which a static build refuses"
		}
	}
	return ""
}

// warnUncarried logs each rule Redirects leaves out: it is served, but a static
// host never sees it.
func (p *Plugin) warnUncarried(host collage.Host) {
	for i := range p.rules {
		if reason := uncarried(&p.rules[i]); reason != "" {
			host.Logger().Warn("redirects: a static build cannot carry a rule; it is served but not exported", "rule", p.rules[i].From, "at", p.rules[i].source, "reason", reason)
		}
	}
}
