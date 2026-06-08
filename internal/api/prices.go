package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// Auto price fetch for trading holdings (stocks/ETFs).
//
// Source: Yahoo Finance's public chart endpoint. No API key, free, serves
// NSE (.NS) and BSE (.BO) in INR. We picked it because Twelve Data's free
// tier paywalls Indian equities ("available starting with the Grow plan"),
// and a personal PKMS shouldn't carry a market-data subscription. The trade
// is reliability: it's undocumented and unversioned. Everything below is
// isolated to fetchPrice/validateSymbol so swapping to a keyed provider later
// is a two-function change.
//
// Driven chunk-per-ping: Cloud Scheduler hits POST /internal/prices/run on a
// short window a few times a day, each hit refreshes only the priceChunkSize
// stalest holdings and returns in ~1s. No in-process sleep, no held-open
// request — the instance wakes, does a chunk, scales back to zero. Suggested
// schedule (Asia/Kolkata, weekdays): `0-4 10,13,16 * * 1-5`.

const (
	yahooChartBase = "https://query1.finance.yahoo.com/v8/finance/chart/"
	priceChunkSize = 8
	// Yahoo rejects requests with no/blank User-Agent; identify politely.
	priceUserAgent = "Mozilla/5.0 (compatible; Sajni/1.0; +https://ohmysajni.com)"
)

var priceHTTPClient = &http.Client{Timeout: 10 * time.Second}

// exchangeSuffix maps a stored exchange to Yahoo's symbol suffix. NSE is the
// default; anything unrecognised falls through to NSE.
func exchangeSuffix(exchange string) string {
	switch strings.ToUpper(strings.TrimSpace(exchange)) {
	case "BSE":
		return ".BO"
	default:
		return ".NS"
	}
}

// yahooChart is the subset of the chart response we read.
type yahooChart struct {
	Chart struct {
		Result []struct {
			Meta struct {
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				Currency           string  `json:"currency"`
			} `json:"meta"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// fetchPrice returns the last regular-market price for symbol on exchange. A
// definitive not-found (bad/delisted symbol) surfaces as a non-nil error whose
// message is stored in price_error; callers treat any error as "no fresh price
// this round".
func fetchPrice(ctx context.Context, symbol, exchange string) (float64, error) {
	ysym := strings.ToUpper(strings.TrimSpace(symbol)) + exchangeSuffix(exchange)
	u := yahooChartBase + url.PathEscape(ysym) + "?interval=1d&range=1d"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", priceUserAgent)
	resp, err := priceHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, fmt.Errorf("rate limited")
	}
	var yc yahooChart
	if err := json.NewDecoder(resp.Body).Decode(&yc); err != nil {
		return 0, err
	}
	if yc.Chart.Error != nil {
		msg := yc.Chart.Error.Description
		if msg == "" {
			msg = yc.Chart.Error.Code
		}
		return 0, fmt.Errorf("%s", msg)
	}
	if len(yc.Chart.Result) == 0 {
		return 0, fmt.Errorf("no data for %s", ysym)
	}
	p := yc.Chart.Result[0].Meta.RegularMarketPrice
	if p <= 0 {
		return 0, fmt.Errorf("no price for %s", ysym)
	}
	return p, nil
}

// validateSymbol confirms symbol resolves on exchange by fetching its price —
// the same call the cron makes, so "valid" means "priceable". Used by
// createInvestment on add (validate once on submit). A clear message is
// returned for the UI when the ticker is wrong.
func validateSymbol(ctx context.Context, symbol, exchange string) error {
	if _, err := fetchPrice(ctx, symbol, exchange); err != nil {
		return fmt.Errorf("couldn't find %s on %s — check the symbol", strings.ToUpper(symbol), strings.ToUpper(exchange))
	}
	return nil
}

// RegisterPriceCronHandler mounts the unauthenticated webhook Cloud Scheduler
// hits to refresh one chunk of holdings. Header X-Price-Cron must match
// PRICE_CRON_SECRET. Mirror of the insight/reminder cron handlers; call from
// main once the root mux exists.
func RegisterPriceCronHandler(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /internal/prices/run", priceCronHandler(deps))
}

func priceCronHandler(deps Deps) http.HandlerFunc {
	expected := os.Getenv("PRICE_CRON_SECRET")
	return func(w http.ResponseWriter, r *http.Request) {
		if expected == "" || r.Header.Get("X-Price-Cron") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		n, err := RunPriceCron(r.Context(), deps)
		if err != nil {
			log.Warn().Err(err).Msg("price cron failed")
			http.Error(w, "error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, 200, map[string]int{"updated": n})
	}
}

// RunPriceCron refreshes the priceChunkSize stalest open stock/ETF holdings
// across all users — one price fetch each. Stalest-first (price_at ASC NULLS
// FIRST) means repeated pings sweep the whole book fairly, and a holding never
// starves: price_at is advanced on every attempt (success OR error), so a
// permanently-bad symbol rotates out instead of blocking the queue.
// Idempotent — extra pings after everything is fresh are cheap.
func RunPriceCron(ctx context.Context, deps Deps) (int, error) {
	d := deps.DB
	rows, err := d.Query(`SELECT id, symbol, exchange, quantity
		FROM fin_investments
		WHERE type IN ('stock','etf') AND status = 'open' AND symbol <> ''
		ORDER BY price_at ASC NULLS FIRST, id ASC
		LIMIT $1`, priceChunkSize)
	if err != nil {
		return 0, err
	}
	type holding struct {
		id       int64
		symbol   string
		exchange string
		qty      float64
	}
	var batch []holding
	for rows.Next() {
		var h holding
		rows.Scan(&h.id, &h.symbol, &h.exchange, &h.qty)
		batch = append(batch, h)
	}
	rows.Close()

	updated := 0
	for _, h := range batch {
		price, ferr := fetchPrice(ctx, h.symbol, h.exchange)
		if ferr != nil {
			// Record the failure but advance price_at so the queue rotates.
			d.Exec(`UPDATE fin_investments SET price_error = $1, price_at = NOW(), last_updated = NOW() WHERE id = $2`,
				ferr.Error(), h.id)
			continue
		}
		// quantity > 0 → mark-to-market the holding; legacy untracked holdings
		// (qty 0) still record the price for display without zeroing value.
		d.Exec(`UPDATE fin_investments
			SET last_price = $1,
			    current_value = CASE WHEN quantity > 0 THEN quantity * $1 ELSE current_value END,
			    price_error = '', price_at = NOW(), last_updated = NOW()
			WHERE id = $2`, price, h.id)
		updated++
	}
	return updated, nil
}
