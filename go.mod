// A collage plugin for redirects kept in a file rather than in code — what a site
// migration leaves behind — served before routing, and written for a static host
// as a _redirects file in the format Netlify and Cloudflare Pages read.
//
// It requires collage the way any consumer does, and reaches nothing the framework
// does not offer every plugin.
module github.com/Elagoht/collage-redirects

go 1.26

require github.com/Elagoht/collage v0.24.0
