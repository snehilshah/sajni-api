package api

import (
	"context"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Link preview: fetch a URL server-side (CORS blocks the browser) and pull
// a human title so the editor can turn a bare link into [title](url).
// Authenticated route; guarded against SSRF (scheme + private-IP checks,
// timeout, capped body).

func registerLinkRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /api/links/preview", linkPreview(deps))
}

var (
	ogTitleRe = regexp.MustCompile(`(?is)<meta[^>]+property\s*=\s*["']og:title["'][^>]*>`)
	contentRe = regexp.MustCompile(`(?is)content\s*=\s*["']([^"']*)["']`)
	titleRe   = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	wsRe      = regexp.MustCompile(`\s+`)
)

// previewClient refuses redirects to keep the SSRF guard meaningful (a
// public URL can't bounce us to an internal one) and caps the dial time.
var previewClient = &http.Client{
	Timeout: 6 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		if err := guardURL(req.URL); err != nil {
			return err
		}
		return nil
	},
}

func linkPreview(_ Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimSpace(queryParam(r, "url"))
		if raw == "" {
			errJSON(w, 400, "missing url")
			return
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errJSON(w, 400, "invalid url")
			return
		}
		if err := guardURL(u); err != nil {
			errJSON(w, 400, "url not allowed")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		req.Header.Set("User-Agent", "SajniBot/1.0 (+https://ohmysajni.com)")
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		resp, err := previewClient.Do(req)
		if err != nil {
			errJSON(w, 502, "fetch failed")
			return
		}
		defer resp.Body.Close()

		// Only parse HTML; cap the read so a huge page can't exhaust memory.
		ct := resp.Header.Get("Content-Type")
		title := ""
		if strings.Contains(ct, "html") || ct == "" {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
			title = extractTitle(string(body))
		}
		if title == "" {
			title = u.Host
		}
		writeJSON(w, 200, map[string]string{
			"url":   u.String(),
			"title": title,
			"host":  u.Host,
		})
	}
}

// extractTitle prefers og:title, then <title>. Returns a cleaned,
// length-capped string.
func extractTitle(body string) string {
	if tag := ogTitleRe.FindString(body); tag != "" {
		if m := contentRe.FindStringSubmatch(tag); len(m) == 2 {
			if t := clean(m[1]); t != "" {
				return t
			}
		}
	}
	if m := titleRe.FindStringSubmatch(body); len(m) == 2 {
		return clean(m[1])
	}
	return ""
}

func clean(s string) string {
	s = html.UnescapeString(s)
	s = wsRe.ReplaceAllString(strings.TrimSpace(s), " ")
	if len(s) > 200 {
		s = strings.TrimSpace(s[:200]) + "…"
	}
	return s
}

// guardURL blocks loopback / private / link-local destinations so the
// preview fetch can't be pointed at internal services.
func guardURL(u *url.URL) error {
	host := u.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(host, ".local") {
		return errBlocked
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return errBlocked
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return errBlocked
		}
	}
	return nil
}

var errBlocked = &blockedErr{}

type blockedErr struct{}

func (*blockedErr) Error() string { return "blocked host" }
