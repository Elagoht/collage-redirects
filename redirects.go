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
// A static host cannot run the middleware, so the plugin also serves the rules as
// /_redirects, in the format Netlify and Cloudflare Pages read, and a static build
// writes it beside the pages.
package redirects

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

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
	// NoRedirectsFile leaves out /_redirects, the copy of the rules a static
	// host reads.
	NoRedirectsFile bool `json:"noRedirectsFile"`
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

// New returns a plugin with opts as its starting point, which the application's
// own configuration is then decoded over.
func New(opts Options) *Plugin { return &Plugin{opts: opts} }

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.1.2" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

// Init reads the rules, refuses any that are wrong, warns of any that hide a page,
// and serves them.
func (p *Plugin) Init(ctx context.Context, host collage.Host) error {
	if err := host.Config(&p.opts); err != nil {
		return err
	}
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

	if !p.opts.NoRedirectsFile {
		doc := collage.NewDocument(Name, "text/plain; charset=utf-8").
			AtRoot("/_redirects").
			WithBody(p.RedirectsFile()).
			Build()
		if err := host.RegisterDocument(doc); err != nil {
			return fmt.Errorf("redirects: %w", err)
		}
	}
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

// RedirectsFile returns the rules in the _redirects format Netlify and Cloudflare
// Pages read. A 410 has no equivalent every such host understands, so it is written
// as a comment.
func (p *Plugin) RedirectsFile() []byte {
	var b bytes.Buffer
	b.WriteString("# Written by elagoht/redirects from the application's rules.\n")
	for _, r := range p.rules {
		if r.Status == http.StatusGone {
			fmt.Fprintf(&b, "# %s is gone (410)\n", r.From)
			continue
		}
		fmt.Fprintf(&b, "%s %s %d\n", r.From, r.To, r.Status)
	}
	return b.Bytes()
}
