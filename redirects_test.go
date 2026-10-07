package redirects_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	redirects "github.com/Elagoht/collage-redirects"
	"github.com/Elagoht/collage/pkg/collage"
)

const file = `# the old blog
/blog/*          /posts/:splat
/about-us        /about          301   # renamed
/summer-sale     /sale?from=summer#top 302
/moved           https://new.example/moved 308
/old-product     -               410

/docs/*          https://docs.example/:splat 307
`

func app(t *testing.T, opts redirects.Options, config map[string]json.RawMessage, log *bytes.Buffer) *collage.App {
	t.Helper()
	cfg := &collage.Config{
		Server:       collage.ServerConfig{Host: "localhost", Port: 3000},
		Template:     collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>page</p>`)}}, Root: "t"},
		Plugins:      []collage.Plugin{redirects.New(opts)},
		PluginConfig: config,
	}
	if log == nil {
		log = new(bytes.Buffer)
	}
	cfg.Logger = slog.New(slog.NewTextHandler(log, nil))
	a, err := collage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"about", "sale", "home"} {
		path := "/" + name
		if name == "home" {
			path = "/"
		}
		if err := a.RegisterPage(collage.NewPage(name).WithContent(collage.NewFragment(name, "p.html").Build()).WithPath("en", path).Build()); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func withFile(body string) redirects.Options {
	return redirects.Options{FS: fstest.MapFS{"redirects.txt": {Data: []byte(body)}}}
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestRedirects(t *testing.T) {
	h := app(t, withFile(file), nil, nil).Handler()
	for _, c := range []struct {
		method, from string
		status       int
		to           string
	}{
		{http.MethodGet, "/about-us", 301, "/about"},
		{http.MethodGet, "/about-us/", 301, "/about"},
		{http.MethodGet, "/about-us?ref=mail&x=1", 301, "/about?ref=mail&x=1"},
		{http.MethodPost, "/about-us", 301, "/about"},
		{http.MethodGet, "/summer-sale?utm=x", 302, "/sale?from=summer&utm=x#top"},
		{http.MethodGet, "/moved", 308, "https://new.example/moved"},
		{http.MethodGet, "/blog/2020/hello", 301, "/posts/2020/hello"},
		{http.MethodGet, "/blog/caf%C3%A9?p=2", 301, "/posts/caf%C3%A9?p=2"},
		{http.MethodGet, "/blog", 301, "/posts/"},
		{http.MethodGet, "/blog/", 301, "/posts/"},
		{http.MethodGet, "/docs/a/b", 307, "https://docs.example/a/b"},
		// collage cleans a doubled slash before the rules see it, so a splat
		// never begins with one there; the rule's own guard stays behind that.
		{http.MethodGet, "/blog//evil.example/x", 301, "/blog/evil.example/x"},
		{http.MethodGet, "/blog/evil.example/x", 301, "/posts/evil.example/x"},
	} {
		rec := get(h, c.method, c.from)
		if rec.Code != c.status || rec.Header().Get("Location") != c.to {
			t.Errorf("%s %s = %d %q, want %d %q", c.method, c.from, rec.Code, rec.Header().Get("Location"), c.status, c.to)
		}
	}
	if rec := get(h, http.MethodGet, "/old-product"); rec.Code != http.StatusGone {
		t.Errorf("/old-product = %d, want 410", rec.Code)
	}
	for _, path := range []string{"/about", "/"} {
		if rec := get(h, http.MethodGet, path); rec.Code != http.StatusOK {
			t.Errorf("%s = %d, want the page", path, rec.Code)
		}
	}
	// A prefix is a path segment, not a string: /blog/* does not take /blogger.
	if rec := get(h, http.MethodGet, "/blogger"); rec.Code != http.StatusNotFound {
		t.Errorf("/blogger = %d %q, want no redirect", rec.Code, rec.Header().Get("Location"))
	}
}

// A gone page is answered with the site's own not-found page, status 410, not a
// line of text.
func TestGoneServesNotFoundPage(t *testing.T) {
	cfg := &collage.Config{
		Server: collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{
			"t/p.html":       {Data: []byte(`<p>page</p>`)},
			"t/missing.html": {Data: []byte(`<h1>Nothing here</h1>`)},
		}, Root: "t"},
		Plugins: []collage.Plugin{redirects.New(withFile(file))},
		Logger:  slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)),
	}
	a, err := collage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RegisterNotFoundPage(collage.NewPage("missing").WithContent(collage.NewFragment("missing", "missing.html").Build()).Build()); err != nil {
		t.Fatal(err)
	}
	rec := get(a.Handler(), http.MethodGet, "/old-product")
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "<h1>Nothing here</h1>") {
		t.Errorf("/old-product = %d %q, want 410 with the not-found page", rec.Code, rec.Body.String())
	}
	// A path no rule names gets the same page, at 404.
	if rec := get(a.Handler(), http.MethodGet, "/nowhere"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Nothing here") {
		t.Errorf("/nowhere = %d %q", rec.Code, rec.Body.String())
	}
}

// The plugin no longer writes a copy of its rules: GET /_redirects finds
// nothing, no document is registered, and a build with no deploy plugin writes
// no _redirects file.
func TestNoRedirectsDocument(t *testing.T) {
	a := app(t, withFile(file), nil, nil)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if code := get(a.Handler(), http.MethodGet, "/_redirects").Code; code != http.StatusNotFound {
		t.Errorf("/_redirects = %d, want 404", code)
	}
	for _, doc := range a.Documents() {
		t.Errorf("a document is registered: %q", doc.Name)
	}
	out := t.TempDir()
	b, err := collage.NewBuilder(app(t, withFile(file), nil, nil), collage.BuildOptions{OutDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "_redirects")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the build wrote _redirects: %v", err)
	}
}

// fileRules is what Redirects() hands a build for the file fixture: every rule,
// a 410 with no destination, and a prefix as the path itself and a catch-all
// under it.
var fileRules = []collage.BuiltRedirect{
	{From: "/blog", To: "/posts/", Status: 301},
	{From: "/blog/{rest...}", To: "/posts/{rest}", Status: 301},
	{From: "/about-us", To: "/about", Status: 301},
	{From: "/summer-sale", To: "/sale?from=summer#top", Status: 302},
	{From: "/moved", To: "https://new.example/moved", Status: 308},
	{From: "/old-product", To: "", Status: 410},
	{From: "/docs", To: "https://docs.example/", Status: 307},
	{From: "/docs/{rest...}", To: "https://docs.example/{rest}", Status: 307},
}

func started(t *testing.T, opts redirects.Options, log *bytes.Buffer) *redirects.Plugin {
	t.Helper()
	p := redirects.New(opts)
	cfg := &collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>page</p>`)}}, Root: "t"},
		Plugins:  []collage.Plugin{p},
	}
	if log == nil {
		log = new(bytes.Buffer)
	}
	cfg.Logger = slog.New(slog.NewTextHandler(log, nil))
	a, err := collage.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRedirectSource(t *testing.T) {
	var _ collage.RedirectSource = redirects.New(redirects.Options{})
	got := started(t, withFile(file), nil).Redirects()
	if !slices.Equal(got, fileRules) {
		t.Errorf("Redirects() =\n%+v\nwant\n%+v", got, fileRules)
	}
}

// Each form the plugin reads, in the router's spelling.
func TestRedirectsMapping(t *testing.T) {
	for _, c := range []struct {
		name  string
		rules []redirects.Rule
		want  []collage.BuiltRedirect
	}{
		{"exact, status defaulted", []redirects.Rule{{From: "/a", To: "/b"}},
			[]collage.BuiltRedirect{{From: "/a", To: "/b", Status: 301}}},
		{"trailing slash kept as written", []redirects.Rule{{From: "/a/", To: "/b/", Status: 308}},
			[]collage.BuiltRedirect{{From: "/a/", To: "/b/", Status: 308}}},
		{"unescaped path", []redirects.Rule{{From: "/café", To: "/caf%C3%A9-new"}},
			[]collage.BuiltRedirect{{From: "/café", To: "/caf%C3%A9-new", Status: 301}}},
		{"gone, \"-\"", []redirects.Rule{{From: "/a", To: "-", Status: 410}},
			[]collage.BuiltRedirect{{From: "/a", Status: 410}}},
		{"gone, empty", []redirects.Rule{{From: "/a", Status: 410}},
			[]collage.BuiltRedirect{{From: "/a", Status: 410}}},
		{"prefix with :splat", []redirects.Rule{{From: "/x/*", To: "/y/:splat", Status: 302}},
			[]collage.BuiltRedirect{{From: "/x", To: "/y/", Status: 302}, {From: "/x/{rest...}", To: "/y/{rest}", Status: 302}}},
		{"prefix without :splat", []redirects.Rule{{From: "/x/*", To: "/y"}},
			[]collage.BuiltRedirect{{From: "/x", To: "/y", Status: 301}, {From: "/x/{rest...}", To: "/y", Status: 301}}},
		{"prefix, :splat twice and in a query", []redirects.Rule{{From: "/x/*", To: "https://e.example/:splat?from=:splat"}},
			[]collage.BuiltRedirect{{From: "/x", To: "https://e.example/?from=", Status: 301}, {From: "/x/{rest...}", To: "https://e.example/{rest}?from={rest}", Status: 301}}},
		{"prefix gone", []redirects.Rule{{From: "/x/*", To: "-", Status: 410}},
			[]collage.BuiltRedirect{{From: "/x", Status: 410}, {From: "/x/{rest...}", Status: 410}}},
		{"root prefix", []redirects.Rule{{From: "/*", To: "https://new.example/:splat"}},
			[]collage.BuiltRedirect{{From: "/", To: "https://new.example/", Status: 301}, {From: "/{rest...}", To: "https://new.example/{rest}", Status: 301}}},
		// /x itself is the earlier rule's, as the middleware's first match is:
		// the prefix brings only its catch-all.
		{"prefix after its own path", []redirects.Rule{{From: "/x", To: "/a"}, {From: "/x/*", To: "/b/:splat"}},
			[]collage.BuiltRedirect{{From: "/x", To: "/a", Status: 301}, {From: "/x/{rest...}", To: "/b/{rest}", Status: 301}}},
		{"prefix after its own path, slashed", []redirects.Rule{{From: "/x/", To: "/a"}, {From: "/x/*", To: "/b"}},
			[]collage.BuiltRedirect{{From: "/x/", To: "/a", Status: 301}, {From: "/x/{rest...}", To: "/b", Status: 301}}},
	} {
		got := started(t, redirects.Options{Rules: c.rules}, nil).Redirects()
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: Redirects() =\n%+v\nwant\n%+v", c.name, got, c.want)
		}
	}
}

// A rule the router's grammar cannot say is served as before, left out of the
// build, and warned about when the application starts.
func TestRedirectsLeaveOutWhatTheRouterCannotSay(t *testing.T) {
	for _, c := range []struct {
		rule   redirects.Rule
		reason string
	}{
		{redirects.Rule{From: "/a{b}", To: "/c"}, "{"},
		{redirects.Rule{From: "/a}", To: "/c"}, "{"},
		{redirects.Rule{From: "/x{y}/*", To: "/c/:splat"}, "{"},
		{redirects.Rule{From: "/a", To: "/b{c}"}, "{"},
		{redirects.Rule{From: "/x/*", To: "/{rest}/:splat"}, "{"},
		{redirects.Rule{From: "/a//b", To: "/c"}, "empty segment"},
		{redirects.Rule{From: "/a//*", To: "/c"}, "empty segment"},
		{redirects.Rule{From: "/x/*", To: "https://:splat@e.example/"}, "host"},
	} {
		var log bytes.Buffer
		opts := redirects.Options{Rules: []redirects.Rule{c.rule, {From: "/kept", To: "/k"}}}
		got := started(t, opts, &log).Redirects()
		want := []collage.BuiltRedirect{{From: "/kept", To: "/k", Status: 301}}
		if !slices.Equal(got, want) {
			t.Errorf("%+v: Redirects() = %+v, want only /kept", c.rule, got)
		}
		if !strings.Contains(log.String(), "a static build cannot carry") || !strings.Contains(log.String(), "Rules[0]") || !strings.Contains(log.String(), c.reason) {
			t.Errorf("%+v: no warning naming it and %q:\n%s", c.rule, c.reason, log.String())
		}
	}
	// Still served.
	h := app(t, redirects.Options{Rules: []redirects.Rule{{From: "/a{b}", To: "/c"}}}, nil, nil).Handler()
	if rec := get(h, http.MethodGet, "/a%7Bb%7D"); rec.Code != 301 || rec.Header().Get("Location") != "/c" {
		t.Errorf("/a{b} = %d %q, want 301 /c", rec.Code, rec.Header().Get("Location"))
	}
}

// capture is a plugin that keeps what the finished build hands its hook.
type capture struct{ ev *collage.BuildFinishedEvent }

func (*capture) Name() string                             { return "test/capture" }
func (*capture) Version() string                          { return "0.0.0" }
func (*capture) Init(context.Context, collage.Host) error { return nil }
func (*capture) Shutdown(context.Context) error           { return nil }
func (c *capture) OnBuildFinished(_ context.Context, ev *collage.BuildFinishedEvent) error {
	c.ev = ev
	return nil
}

// The core takes every rule as it is handed over: a real build, no error, and
// the hook sees the rules stamped with the plugin's name.
func TestBuildCarriesTheRules(t *testing.T) {
	c := &capture{}
	a, err := collage.New(&collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>page</p>`)}}, Root: "t"},
		Plugins:  []collage.Plugin{redirects.New(withFile(file)), c},
		Logger:   slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RegisterPage(collage.NewPage("about").WithContent(collage.NewFragment("about", "p.html").Build()).WithPath("en", "/about").Build()); err != nil {
		t.Fatal(err)
	}
	b, err := collage.NewBuilder(a, collage.BuildOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background()); err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := make([]collage.BuiltRedirect, len(fileRules))
	for i, r := range fileRules {
		r.Source = redirects.Name
		want[i] = r
	}
	if c.ev == nil || !slices.Equal(c.ev.Redirects, want) {
		t.Errorf("the hook's redirects =\n%+v\nwant\n%+v", c.ev, want)
	}
}

// A root prefix takes every path, so a build that writes any file fails, as a
// redirect over a written file always does; the server is unaffected.
func TestRootPrefixFailsTheBuild(t *testing.T) {
	a := app(t, redirects.Options{Rules: []redirects.Rule{{From: "/*", To: "https://new.example/:splat"}}}, nil, nil)
	b, err := collage.NewBuilder(a, collage.BuildOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background()); !errors.Is(err, collage.ErrRedirectShadowsFile) {
		t.Errorf("Build = %v, want ErrRedirectShadowsFile", err)
	}
}

// A control character in From or To is refused, from Go and from configuration
// alike, as the core would refuse it in a build; a CRLF file still reads.
func TestControlCharacters(t *testing.T) {
	for _, r := range []redirects.Rule{
		{From: "/a\r", To: "/b"},
		{From: "/a\nb", To: "/b"},
		{From: "/a\x00", To: "/b"},
		{From: "/a\x7f", To: "/b"},
		{From: "/a\u0085", To: "/b"},
		{From: "/a\u2028", To: "/b"},
		{From: "/a", To: "/b\r\nSet-Cookie: x=1"},
		{From: "/a", To: "/b\x1b"},
		{From: "/a", To: "https://e.example/\u2029"},
		{From: "/x/*", To: "/b\n:splat"},
	} {
		a := app(t, redirects.Options{Rules: []redirects.Rule{r}}, nil, nil)
		if err := a.Start(); !errors.Is(err, redirects.ErrInvalidRule) || !strings.Contains(err.Error(), "control character") {
			t.Errorf("%q -> %q: Start = %v, want ErrInvalidRule for a control character", r.From, r.To, err)
		}
	}
	for _, raw := range []string{
		`{"rules":[{"from":"/a\r","to":"/b"}]}`,
		`{"rules":[{"from":"/a","to":"/b\nLocation: /evil"}]}`,
		`{"rules":[{"from":"/a","to":"/b\u0000"}]}`,
		`{"rules":[{"from":"/a\u2028","to":"/b"}]}`,
	} {
		config := map[string]json.RawMessage{redirects.Name: json.RawMessage(raw)}
		if err := app(t, redirects.Options{}, config, nil).Start(); !errors.Is(err, redirects.ErrInvalidRule) || !strings.Contains(err.Error(), "control character") {
			t.Errorf("%s: Start = %v, want ErrInvalidRule for a control character", raw, err)
		}
	}
	if err := app(t, withFile("/a\x01 /b\n"), nil, nil).Start(); !errors.Is(err, redirects.ErrInvalidRule) || !strings.Contains(err.Error(), "control character") {
		t.Errorf("a file with a control character: Start = %v", err)
	}
	// U+2028 is a space to the file's reader: it separates fields, and never
	// reaches a rule.
	p := started(t, withFile("/a /b\u2028302\n"), nil)
	if got, want := p.Redirects(), []collage.BuiltRedirect{{From: "/a", To: "/b", Status: 302}}; !slices.Equal(got, want) {
		t.Errorf("U+2028 file: Redirects() = %+v, want %+v", got, want)
	}
	h := app(t, withFile("/a /b 302\r\n/c /d\r\n"), nil, nil).Handler()
	if rec := get(h, http.MethodGet, "/a"); rec.Code != 302 || rec.Header().Get("Location") != "/b" {
		t.Errorf("CRLF file: /a = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// Rules given in Go or configuration join the file's.
func TestRulesAndConfiguration(t *testing.T) {
	opts := withFile("/a /b\n")
	opts.Rules = []redirects.Rule{{From: "/c", To: "/d", Status: 302}}
	config := map[string]json.RawMessage{redirects.Name: json.RawMessage(`{"rules":[{"from":"/e","to":"/f"}]}`)}
	h := app(t, opts, config, nil).Handler()
	// Configuration replaces the list given in Go, as JSON decoding a slice does.
	for from, want := range map[string]string{"/a": "/b", "/e": "/f"} {
		if loc := get(h, http.MethodGet, from).Header().Get("Location"); loc != want {
			t.Errorf("%s -> %q, want %q", from, loc, want)
		}
	}

	// A file named in configuration, read from the working directory.
	dir := t.TempDir()
	path := filepath.Join(dir, "moves.txt")
	if err := os.WriteFile(path, []byte("/g /h 307\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config = map[string]json.RawMessage{redirects.Name: json.RawMessage(`{"file":` + strings.ReplaceAll(`"`+path+`"`, `\`, `\\`) + `}`)}
	if rec := get(app(t, redirects.Options{}, config, nil).Handler(), http.MethodGet, "/g"); rec.Code != 307 || rec.Header().Get("Location") != "/h" {
		t.Errorf("/g = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// Anything wrong stops the application from starting, and says where.
func TestRefusals(t *testing.T) {
	for body, want := range map[string]string{
		"/a\n":                     "redirects.txt:1",
		"# ok\n/a /b 301 extra\n":  "redirects.txt:2",
		"/a /b 303\n":              "status 303",
		"/a /b abc\n":              "not a number",
		"/a -\n":                   "410",
		"/a /b 410\n":              "gone and goes nowhere",
		"a /b\n":                   "beginning with one /",
		"/a?x=1 /b\n":              "query",
		"/a/*/b /c\n":              "* may only end",
		"/a /b/:splat\n":           ":splat",
		"/a //evil.example\n":      "http(s) URL",
		"/a /\\evil.example\n":     "another host",
		"/a javascript:alert(1)\n": "http(s) URL",
		"/a /b\n/a /c\n":           "redirects.txt:2: /a is never reached",
		"/a/* /x\n/a/b /y\n":       "redirects.txt:2: /a/b is never reached",
		"/a /b\n/b /a\n":           "circle",
		"/a /a/\n":                 "circle",
		"/x/* /x/y/:splat\n":       "circle",
		"/a /b\n/b /c\n/c /a\n":    "circle",
	} {
		a := app(t, withFile(body), nil, nil)
		err := a.Start()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: Start = %v, want an error containing %q", body, err, want)
		}
		if code := get(a.Handler(), http.MethodGet, "/").Code; code != http.StatusServiceUnavailable {
			t.Errorf("%q: %d, want 503", body, code)
		}
	}
	a := app(t, redirects.Options{Rules: []redirects.Rule{{From: "/a", To: "/b", Status: 404}}}, nil, nil)
	if err := a.Start(); !errors.Is(err, redirects.ErrInvalidRule) || !strings.Contains(err.Error(), "Rules[0]") {
		t.Errorf("Start = %v", err)
	}
	if err := app(t, redirects.Options{FS: fstest.MapFS{}}, nil, nil).Start(); err == nil {
		t.Error("a missing file started the application")
	}
	// A chain is not a circle.
	if err := app(t, withFile("/a /b\n/b /c\n"), nil, nil).Start(); err != nil {
		t.Errorf("a chain was refused: %v", err)
	}
}

// A rule over a page's path is allowed, and warned about.
func TestShadowedPageWarns(t *testing.T) {
	var log bytes.Buffer
	a := app(t, withFile("/about /about-us\n"), nil, &log)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "a rule hides a page") || !strings.Contains(log.String(), "page=about") {
		t.Errorf("no warning:\n%s", log.String())
	}
}

var _ collage.BuildFinishedHook = redirects.New(redirects.Options{})

// A rule the build leaves out, and a plugin rule covering a page's own redirect,
// are warnings in the build's findings as well as at startup.
func TestBuildFindings(t *testing.T) {
	var log bytes.Buffer
	a, err := collage.New(&collage.Config{
		Server:   collage.ServerConfig{Host: "localhost", Port: 3000},
		Template: collage.TemplateConfig{FS: fstest.MapFS{"t/p.html": {Data: []byte(`<p>page</p>`)}}, Root: "t"},
		Plugins: []collage.Plugin{redirects.New(redirects.Options{Rules: []redirects.Rule{
			{From: "/a{b}", To: "/c"},
			{From: "/blog/*", To: "/posts/:splat"},
		}})},
		Logger: slog.New(slog.NewTextHandler(&log, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	page := collage.NewPage("about").WithContent(collage.NewFragment("about", "p.html").Build()).
		WithPath("en", "/about").WithPermanentRedirect("/blog/old", "/about").Build()
	if err := a.RegisterPage(page); err != nil {
		t.Fatal(err)
	}
	b, err := collage.NewBuilder(a, collage.BuildOptions{OutDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	report, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for path, text := range map[string]string{
		"/a{b}":     "Rules[0]",
		"/blog/old": "/blog/*",
	} {
		found := slices.ContainsFunc(report.Findings, func(f collage.Finding) bool {
			return f.Rule == "redirects-not-exported" && f.Level == collage.FindingWarning && f.Path == path && strings.Contains(f.Message, text)
		})
		if !found {
			t.Errorf("no redirects-not-exported warning at %s naming %q: %+v", path, text, report.Findings)
		}
	}
	if !strings.Contains(log.String(), "a rule covers a page's redirect") || !strings.Contains(log.String(), "/blog/old") {
		t.Errorf("no startup warning for the covered redirect:\n%s", log.String())
	}
}
