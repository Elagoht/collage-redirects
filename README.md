# elagoht/redirects

A collage plugin for redirects kept in a file rather than in code — what a site
migration leaves behind, hundreds of old addresses and where each one went —
served before routing, and written for a static host as a `_redirects` file.

```go
//go:embed redirects.txt
var siteFS embed.FS

app, err := collage.New(&collage.Config{
	Plugins: []collage.Plugin{redirects.New(redirects.Options{FS: siteFS})},
})
```

Requires collage v0.24.0 or later.

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

A static host cannot run the middleware, so the plugin also serves the rules at
`/_redirects`, in the format Netlify and Cloudflare Pages read, and a static build
writes it at the root of the output:

```
# Written by elagoht/redirects from the application's rules.
/blog/* /posts/:splat 301
/about-us /about 301
# /old-product is gone (410)
```

A `410` is written as a comment: neither host has a way to say "gone" every other
understands. `noRedirectsFile` leaves `/_redirects` out.

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
    "rules": [{ "from": "/careers", "to": "https://jobs.example.com", "status": 302 }],
    "noRedirectsFile": false
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
- `/_redirects` is served by the running application too, which is harmless and
  lets you see what a static host would read. Netlify and Cloudflare Pages differ in
  details the plugin does not paper over: Cloudflare caps the number of rules, and
  each host has its own rules for query strings.

## Changes

### v0.1.1

- A gone page is answered with the site's own not-found page, status 410, through
  collage v0.24.0's `Host.ServeStatus`, rather than a line of text.
- Requires collage v0.24.0.
