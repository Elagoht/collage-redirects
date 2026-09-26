package redirects_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		// A splat cannot turn a path into another host.
		{http.MethodGet, "/blog//evil.example/x", 301, "/posts/evil.example/x"},
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

// What a static host reads: every rule but a 410, which it has no word for.
func TestRedirectsFile(t *testing.T) {
	rec := get(app(t, withFile(file), nil, nil).Handler(), http.MethodGet, "/_redirects")
	want := "# Written by elagoht/redirects from the application's rules.\n" +
		"/blog/* /posts/:splat 301\n" +
		"/about-us /about 301\n" +
		"/summer-sale /sale?from=summer#top 302\n" +
		"/moved https://new.example/moved 308\n" +
		"# /old-product is gone (410)\n" +
		"/docs/* https://docs.example/:splat 307\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("/_redirects = %d\n%s\nwant\n%s", rec.Code, rec.Body.String(), want)
	}
}

// A static build writes _redirects as a file of its own.
func TestStaticBuild(t *testing.T) {
	out := t.TempDir()
	b, err := collage.NewBuilder(app(t, withFile(file), nil, nil), collage.BuildOptions{OutDir: out})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(out, "_redirects"))
	if err != nil || !strings.Contains(string(body), "/blog/* /posts/:splat 301\n") {
		t.Errorf("_redirects = %q, %v", body, err)
	}

	off := withFile(file)
	off.NoRedirectsFile = true
	if code := get(app(t, off, nil, nil).Handler(), http.MethodGet, "/_redirects").Code; code != http.StatusNotFound {
		t.Errorf("NoRedirectsFile: /_redirects = %d", code)
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
