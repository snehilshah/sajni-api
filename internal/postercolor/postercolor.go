// Package postercolor works out one colour per media poster, the "title
// colour" both apps tint a title's row and sheet with. It's computed once
// per poster URL (media.poster_color_of remembers which), right after a save
// and by the quarter-hour sweep for anything still missing, so the clients
// only read a hex string.
package postercolor

import (
	"context"
	"database/sql"
	"fmt"
	"image"
	_ "image/jpeg" // posters are JPEGs
	_ "image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Only the catalogues posters come from are fetched, so a typed-in poster URL
// can't point the server at anything else. Open Library redirects covers to
// archive.org.
func allowed(u *url.URL) bool {
	if u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "image.tmdb.org" || h == "covers.openlibrary.org" || h == "archive.org" || strings.HasSuffix(h, ".archive.org")
}

var client = &http.Client{
	Timeout: 8 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 || !allowed(req.URL) {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

// errSkip marks a poster that will never give a colour (not a catalogue
// image, not decodable); it's remembered so it isn't fetched again.
var errSkip = fmt.Errorf("postercolor: skip")

// Of fetches the poster and returns its colour as "#rrggbb".
func Of(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || !allowed(u) {
		return "", errSkip
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", errSkip
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return "", fmt.Errorf("postercolor: %s", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return "", errSkip
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", errSkip
	}
	return Pick(img), nil
}

// Pick returns the image's dominant vivid colour: pixels are sampled on a
// grid, grouped by hue (greys and near-blacks/whites set aside), and the
// heaviest hue group's average wins. A poster with no colour at all gives
// its average grey.
func Pick(img image.Image) string {
	const grid = 48
	const buckets = 24
	b := img.Bounds()
	var sum [buckets][3]float64
	var weight [buckets]float64
	var all [3]float64
	n := 0.0
	for gy := 0; gy < grid; gy++ {
		for gx := 0; gx < grid; gx++ {
			x := b.Min.X + (gx*b.Dx()+b.Dx()/2)/grid
			y := b.Min.Y + (gy*b.Dy()+b.Dy()/2)/grid
			r16, g16, b16, _ := img.At(x, y).RGBA()
			r, g, bl := float64(r16>>8), float64(g16>>8), float64(b16>>8)
			all[0] += r
			all[1] += g
			all[2] += bl
			n++
			h, s, v := hsv(r, g, bl)
			if s < 0.25 || v < 0.18 || v > 0.97 {
				continue
			}
			i := int(h/360*buckets) % buckets
			w := s * v
			weight[i] += w
			sum[i][0] += r * w
			sum[i][1] += g * w
			sum[i][2] += bl * w
		}
	}
	best := -1
	for i := range weight {
		if weight[i] > 0 && (best < 0 || weight[i] > weight[best]) {
			best = i
		}
	}
	// Too little colour to trust (a black-and-white poster): its grey.
	if best < 0 || weight[best] < 0.02*n {
		return hex(all[0]/n, all[1]/n, all[2]/n)
	}
	w := weight[best]
	return hex(sum[best][0]/w, sum[best][1]/w, sum[best][2]/w)
}

func hsv(r, g, b float64) (h, s, v float64) {
	r, g, b = r/255, g/255, b/255
	mx := math.Max(r, math.Max(g, b))
	mn := math.Min(r, math.Min(g, b))
	v = mx
	d := mx - mn
	if mx > 0 {
		s = d / mx
	}
	if d == 0 {
		return 0, s, v
	}
	switch mx {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	return h, s, v
}

func hex(r, g, b float64) string {
	c := func(v float64) int { return int(math.Round(math.Max(0, math.Min(255, v)))) }
	return fmt.Sprintf("#%02x%02x%02x", c(r), c(g), c(b))
}

// FillOne colours one media row now if its poster changed since it was last
// coloured. Network trouble leaves it for the sweep.
func FillOne(ctx context.Context, d *sql.DB, id int64) {
	var poster, of string
	if err := d.QueryRowContext(ctx, `SELECT poster_url, poster_color_of FROM media WHERE id=$1`, id).Scan(&poster, &of); err != nil || poster == of {
		return
	}
	store(ctx, d, id, poster)
}

// Fill colours up to limit rows whose poster has no colour yet, for every
// user. Returns how many it settled.
func Fill(ctx context.Context, d *sql.DB, limit int) (int, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, poster_url FROM media WHERE poster_url <> poster_color_of ORDER BY updated_at DESC LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type job struct {
		id     int64
		poster string
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.poster); err != nil {
			rows.Close()
			return 0, err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	done := 0
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		if store(ctx, d, j.id, j.poster) {
			done++
		}
	}
	return done, nil
}

// store computes and saves the colour; false when it should be tried again.
func store(ctx context.Context, d *sql.DB, id int64, poster string) bool {
	color := ""
	if poster != "" {
		c, err := Of(ctx, poster)
		if err != nil && err != errSkip {
			return false
		}
		color = c
	}
	// Only if the poster is still the one coloured (an edit may have raced).
	_, err := d.ExecContext(ctx, `UPDATE media SET poster_color=$1, poster_color_of=$2 WHERE id=$3 AND poster_url=$2`, color, poster, id)
	return err == nil
}
