package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// mediaMeta is the enriched metadata addMediaTool fills when the model adds
// a title without an external_id. Mirrors what the manual Add flow pulls
// from TMDB / Open Library so AI-added entries aren't bare.
type mediaMeta struct {
	ExternalID     string
	PosterURL      string
	Year           int
	Genre          string
	SeasonsTotal   int
	EpisodesTotal  int
	SeasonEpisodes []int
}

// enrichMediaMeta resolves real metadata for a title by type. Best-effort:
// returns a zero mediaMeta when the relevant API key is unset or nothing
// matches, so the add never fails on enrichment.
func enrichMediaMeta(ctx context.Context, title, mtype string) mediaMeta {
	switch mtype {
	case "book":
		return enrichBook(ctx, title)
	default: // movie / show
		return enrichTMDB(ctx, title, mtype)
	}
}

func httpGetJSON(ctx context.Context, u string, out any) error {
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return json.Unmarshal(body, out)
}

// enrichTMDB searches TMDB, takes the top hit, then pulls its detail record
// for genre names and (for shows) season/episode counts.
func enrichTMDB(ctx context.Context, title, mtype string) mediaMeta {
	apiKey := os.Getenv("TMDB_API_KEY")
	if apiKey == "" || title == "" {
		return mediaMeta{}
	}
	endpoint := "movie"
	if mtype == "show" || mtype == "tv" {
		endpoint = "tv"
	}

	var search struct {
		Results []struct {
			ID           float64 `json:"id"`
			PosterPath   string  `json:"poster_path"`
			ReleaseDate  string  `json:"release_date"`
			FirstAirDate string  `json:"first_air_date"`
		} `json:"results"`
	}
	su := fmt.Sprintf("https://api.themoviedb.org/3/search/%s?api_key=%s&query=%s&page=1",
		endpoint, apiKey, url.QueryEscape(title))
	if err := httpGetJSON(ctx, su, &search); err != nil || len(search.Results) == 0 {
		return mediaMeta{}
	}
	top := search.Results[0]
	m := mediaMeta{ExternalID: fmt.Sprintf("tmdb:%s:%.0f", endpoint, top.ID)}
	if top.PosterPath != "" {
		m.PosterURL = "https://image.tmdb.org/t/p/w300" + top.PosterPath
	}
	release := top.ReleaseDate
	if release == "" {
		release = top.FirstAirDate
	}
	if len(release) >= 4 {
		fmt.Sscanf(release[:4], "%d", &m.Year)
	}

	// Detail record: genre names + (tv) season/episode breakdown.
	var detail struct {
		Genres []struct {
			Name string `json:"name"`
		} `json:"genres"`
		NumberOfSeasons  int `json:"number_of_seasons"`
		NumberOfEpisodes int `json:"number_of_episodes"`
		Seasons          []struct {
			SeasonNumber int `json:"season_number"`
			EpisodeCount int `json:"episode_count"`
		} `json:"seasons"`
	}
	du := fmt.Sprintf("https://api.themoviedb.org/3/%s/%.0f?api_key=%s", endpoint, top.ID, apiKey)
	if err := httpGetJSON(ctx, du, &detail); err == nil {
		genres := make([]string, 0, len(detail.Genres))
		for _, g := range detail.Genres {
			if g.Name != "" {
				genres = append(genres, g.Name)
			}
		}
		m.Genre = strings.Join(genres, ", ")
		if endpoint == "tv" {
			m.SeasonsTotal = detail.NumberOfSeasons
			m.EpisodesTotal = detail.NumberOfEpisodes
			for _, s := range detail.Seasons {
				if s.SeasonNumber == 0 { // skip "Specials"
					continue
				}
				m.SeasonEpisodes = append(m.SeasonEpisodes, s.EpisodeCount)
			}
		}
	}
	return m
}

// enrichBook resolves a book via Open Library's free search API (no key).
func enrichBook(ctx context.Context, title string) mediaMeta {
	if title == "" {
		return mediaMeta{}
	}
	var search struct {
		Docs []struct {
			Key            string   `json:"key"`
			CoverI         int      `json:"cover_i"`
			FirstPublishYr int      `json:"first_publish_year"`
			Subject        []string `json:"subject"`
		} `json:"docs"`
	}
	u := "https://openlibrary.org/search.json?limit=1&q=" + url.QueryEscape(title)
	if err := httpGetJSON(ctx, u, &search); err != nil || len(search.Docs) == 0 {
		return mediaMeta{}
	}
	d := search.Docs[0]
	m := mediaMeta{Year: d.FirstPublishYr}
	if d.Key != "" {
		m.ExternalID = "openlibrary:" + strings.TrimPrefix(d.Key, "/works/")
	}
	if d.CoverI > 0 {
		m.PosterURL = fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", d.CoverI)
	}
	if len(d.Subject) > 0 {
		end := 3
		if len(d.Subject) < end {
			end = len(d.Subject)
		}
		m.Genre = strings.Join(d.Subject[:end], ", ")
	}
	return m
}
