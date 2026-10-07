# elagoht/redirects

A collage plugin for redirects kept in a file rather than in code — what a site
migration leaves behind, hundreds of old addresses and where each one went —
served before routing, and handed to a static build for the host's own
configuration.

```go
//go:embed redirects.txt
var siteFS embed.FS

app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{redirects.New(redirects.Options{FS: siteFS})},
})
```

Requires collage v0.52.0 or later.

## The file

One rule a line: the old path, where it went, and a status.

```
# the old blog
/blog/*          /posts/:splat
/about-us        /about          301
/summer-sale     /sale           302
/moved           https://new.example/moved 308
/old-product     -               410
```

- **From** is a path. Ending in `/*` it is a prefix: `/blog/*` matches `/blog`,
  `/blog/` and everything under it — not `/blogger` — and `:splat` in the target is
  what the `*` matched. A trailing slash on a request does not matter: `/about-us/`
  is `/about-us`. A query cannot be matched.
- **To** is a path on the site or an `http`/`https` URL, and may carry a query and
  a fragment of its own. `-` is a page that is gone.
- **Status** is `301` when left out, or `302`, `307`, `308`, or `410` for a page
  that is gone with no replacement, which is answered `410 Gone` with the site's
  own not-found page.
- `#` starts a comment, on a line of its own or after a rule.

The first rule that matches wins, as on Netlify and Cloudflare Pages. The query
string the reader arrived with is carried over, after the target's own:
`/summer-sale?utm_source=mail` goes to `/sale?utm_source=mail`.

## Refused at startup

A file that is wrong stops the application from starting, and says where:

```
redirects: invalid rule: redirects.txt:14: /blog/2020/* is never reached: /blog/* (redirects.txt:3) matches it first
```

- a control character in a From or a To (a carriage return, a line feed, any other
  control character, or U+2028/U+2029), from the file, Go or configuration;
- a target beginning `/\`, which a browser reads as another host, as it does `//`;
- a line that is not `FROM TO [STATUS]`, a status that is not one of the five, a
  `-` without `410` or a `410` with a target, a `:splat` with no `/*`, a target
  that is neither a path nor an `http(s)` URL;
- a rule no request can reach, because an earlier one matches everything it would —
  the same path twice included;
- rules that send a reader round in a circle — `/a` to `/b` and `/b` to `/a`, or
  `/x/*` to `/x/y/:splat`, which grows forever. A chain, `/a` to `/b` to `/c`, is
  allowed; each hop is its own redirect.

A rule over a page's path is allowed — a page replaced before its code was
removed — but it hides the page, since the middleware runs before routing, so it is
logged as a warning when the application starts.

## Static hosts

A static host cannot run the middleware. The plugin is a collage `RedirectSource`:
a static build asks it for its rules, checks them as the router would, and hands
them, with the pages' own redirects, to the plugin that writes a host's
configuration — [elagoht/deploy](https://github.com/Elagoht/collage-deploy), which
writes `_redirects` for Netlify and Cloudflare Pages, `vercel.json` for Vercel, or
redirect pages for GitHub Pages. Without such a plugin nothing is written.

The rules are handed over in collage's pattern syntax:

| In the plugin | To the build |
|---|---|
| `/about-us /about 301` | From `/about-us`, To `/about`, 301 (From as written, a trailing `/` kept) |
| `/old-product - 410` | From `/old-product`, To empty, 410 |
| `/blog/* /posts/:splat` | From `/blog`, To `/posts/`; and From `/blog/{rest...}`, To `/posts/{rest}` |
| `/blog/* /posts` | From `/blog`, To `/posts`; and From `/blog/{rest...}`, To `/posts` |
| `/blog /x` then `/blog/* /y/:splat` | From `/blog`, To `/x`; and From `/blog/{rest...}`, To `/y/{rest}` |
| `/* https://new.example/:splat` | From `/`, and From `/{rest...}`: a build that writes any file fails |
| `/blog/* /posts/:splat`, request `/blog/a/` | the server sends `/posts/a/`; collage's router drops the trailing `/` before capturing, so read as the build's rule it is `/posts/a`, and each host follows its own reading |
| `/caf%C3%A9 /x` | From `/caf%C3%A9`, as written: a `%` in From is handed over raw, and both the plugin and the router read it as the literal characters, not as an escape; a host may decode it |

A prefix becomes two rules because collage's catch-all needs at least one segment
and the plugin's `/*` matches the path itself too; the path is left out when an
earlier rule already takes it, as the middleware's first match would. A rule
collage's syntax cannot say — a `{` or `}` in From or To, which collage reads as
a placeholder, an empty segment (`/a//b`), or `:splat` in an absolute target's
host — is still served, but left out of the build, with a warning when the
application starts and a `redirects-not-exported` warning in the build's
findings.

## Rules in Go

Rules can also be given in Go or configuration, and are checked on the same terms,
after the file's:

```go
redirects.New(redirects.Options{
	FS: siteFS,
	Rules: []redirects.Rule{
		{From: "/careers", To: "https://jobs.example.com", Status: 302},
		{From: "/press-kit", Status: 410},
	},
})
```

## Configuration

```json
{
  "elagoht/redirects": {
    "file": "redirects.txt",
    "rules": [{ "from": "/careers", "to": "https://jobs.example.com", "status": 302 }]
  }
}
```

With `FS` set, `file` is read from it, `redirects.txt` by default, and a missing
file is an error. Without `FS`, a `file` named in configuration is read from the
working directory, and with neither no file is read. `rules` in configuration
replaces the list given in Go, as decoding JSON into a slice does.

## Limitations

- The rules are read once, when the application starts; a change to the file needs
  a restart.
- Only the URLs `Host.PageURLs` knows are checked for a rule hiding a page. A
  pattern without `WithStaticParams`, and every document, is not.
- A loop through another host — `/a` to `https://example.com/b`, which is this
  site — cannot be seen.
- A rule over a page's path is only a warning to the running application, but a
  static build refuses a redirect over a file it wrote
  (`collage.ErrRedirectShadowsFile`): a site whose export worked with v0.1.x can
  fail to build until the rule or the page goes.
- A rule covering a page's or a document's own redirect — the plugin's `/blog/*`
  and a page's `WithPermanentRedirect("/blog/old", "/elsewhere")` — sends
  `/blog/old` by the rule on the server, since the middleware runs before routing,
  and by the page's redirect on a static host. It is warned about when the
  application starts (pages only) and as a `redirects-not-exported` finding in a
  build (pages and documents). The same From in both exactly fails the build with
  `collage.ErrDuplicateRedirect`, while the server lets the plugin's rule win.
- What each host can carry — 410, 307 and 308, rule limits, query strings — is
  elagoht/deploy's to report, as warnings in the build's findings.

## Changes

### v0.2.0

- Breaking: the plugin no longer serves or writes `/_redirects`. It is a collage
  `RedirectSource`, and a static build hands its rules to a deploy plugin such as
  elagoht/deploy, which writes each host's own format. `Options.NoRedirectsFile`
  and `RedirectsFile()` are gone; a `noRedirectsFile` key in configuration is
  ignored.
- A control character in a rule's From or To is refused at startup, and so is a
  To beginning `/\`, which a browser reads as another host: until now it was
  served.
- A build's findings carry a `redirects-not-exported` warning for each rule left
  out of the build and each page's or document's redirect a rule covers.
- Requires collage v0.52.0.

### v0.1.6

- Retracts v0.1.4, tagged by mistake on the previous release's code. Use v0.1.5 or later. Nothing else changes.

### v0.1.5

- Requires collage v0.50.0. Plugin configuration is read with `collage.PluginConfig`, since `host.Config` is gone. Nothing else changes.

### v0.1.2

- `collage.json`: the plugin described to editors — its template functions,
  snippets and configuration schema — for the Collage Snippets & Highlighter
  extension and any tool reading it.

### v0.1.1

- A gone page is answered with the site's own not-found page, status 410, through
  collage v0.24.0's `Host.ServeStatus`, rather than a line of text.
- Requires collage v0.24.0.
