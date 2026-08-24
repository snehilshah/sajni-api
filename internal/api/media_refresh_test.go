package api

import (
	"context"
	"errors"
	"testing"
	"time"

	mediastatus "sajni/internal/media"
)

func TestDecideMediaRefresh(t *testing.T) {
	tests := []struct {
		name      string
		candidate mediaRefreshCandidate
		details   MediaDetails
		want      mediaRefreshDecision
	}{
		{
			name:      "first metadata is baseline",
			candidate: mediaRefreshCandidate{},
			details:   MediaDetails{SeasonEpisodes: []int{10, 8}},
			want:      mediaRefreshBaseline,
		},
		{
			name:      "higher season count is new season",
			candidate: mediaRefreshCandidate{SeasonEpisodes: []int{10, 8, 8}},
			details:   MediaDetails{SeasonEpisodes: []int{10, 8, 8, 8}},
			want:      mediaRefreshNewSeason,
		},
		{
			name:      "more episodes in same season is not new season",
			candidate: mediaRefreshCandidate{SeasonEpisodes: []int{10, 8, 2}},
			details:   MediaDetails{SeasonEpisodes: []int{10, 8, 8}},
			want:      mediaRefreshChecked,
		},
		{
			name:      "unchanged metadata only checks timestamp",
			candidate: mediaRefreshCandidate{SeasonsTotal: 3},
			details:   MediaDetails{SeasonsTotal: 3},
			want:      mediaRefreshChecked,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := decideMediaRefresh(test.candidate, test.details); got != test.want {
				t.Fatalf("decideMediaRefresh() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestValidateUserMediaStatus(t *testing.T) {
	if err := validateUserMediaStatus(mediastatus.StatusNewSeason, ""); err == nil {
		t.Fatal("client manufactured new_season on create")
	}
	if err := validateUserMediaStatus(mediastatus.StatusNewSeason, "complete"); err == nil {
		t.Fatal("client manufactured new_season on update")
	}
	if err := validateUserMediaStatus(mediastatus.StatusNewSeason, "new_season"); err != nil {
		t.Fatalf("idempotent new_season save failed: %v", err)
	}
	if err := validateUserMediaStatus(mediastatus.StatusInProgress, "new_season"); err != nil {
		t.Fatalf("manual exit from new_season failed: %v", err)
	}
}

type fakeMediaRefreshStore struct {
	candidates []mediaRefreshCandidate
	cutoff     time.Time
	applied    []int64
}

func (store *fakeMediaRefreshStore) staleCompletedShows(
	_ context.Context,
	_ string,
	cutoff time.Time,
	_ int,
) ([]mediaRefreshCandidate, error) {
	store.cutoff = cutoff
	return store.candidates, nil
}

func (store *fakeMediaRefreshStore) applyMediaRefresh(
	_ context.Context,
	_ string,
	candidate mediaRefreshCandidate,
	details MediaDetails,
	_ time.Time,
) (bool, error) {
	store.applied = append(store.applied, candidate.ID)
	return decideMediaRefresh(candidate, details) == mediaRefreshNewSeason, nil
}

func TestRunMediaMetadataRefreshContinuesAfterTMDBFailure(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	store := &fakeMediaRefreshStore{candidates: []mediaRefreshCandidate{
		{ID: 1, Title: "Broken", ExternalID: "tmdb:tv:1", SeasonEpisodes: []int{8}},
		{ID: 2, Title: "Dragon", ExternalID: "tmdb:tv:2", SeasonEpisodes: []int{10, 8, 8}},
	}}
	fetch := func(_ context.Context, externalID, _ string) (MediaDetails, error) {
		if externalID == "tmdb:tv:1" {
			return MediaDetails{}, errors.New("tmdb unavailable")
		}
		return MediaDetails{SeasonEpisodes: []int{10, 8, 8, 8}, EpisodesTotal: 34}, nil
	}

	result, err := runMediaMetadataRefresh(context.Background(), store, "user", "key", now, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(-7 * 24 * time.Hour); !store.cutoff.Equal(want) {
		t.Fatalf("cutoff = %v, want %v", store.cutoff, want)
	}
	if result.Checked != 1 || result.Changed != 1 {
		t.Fatalf("result = %#v, want one checked and changed", result)
	}
	if len(result.Titles) != 1 || result.Titles[0] != "Dragon" {
		t.Fatalf("titles = %#v, want Dragon", result.Titles)
	}
	if len(store.applied) != 1 || store.applied[0] != 2 {
		t.Fatalf("applied = %#v, want only successful show", store.applied)
	}
}
