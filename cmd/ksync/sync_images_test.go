package main

import (
	"slices"
	"testing"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
)

func TestSyncImages(t *testing.T) {
	app := config.App{
		Name:  "api-b",
		Needs: []string{"team-a", "shop"},
		Build: []config.Build{{Image: "ghcr.io/app/api-b"}},
	}
	needed := []config.BuildRef{
		{App: "team-a", Index: 0, Image: "ghcr.io/app/seed"},
		{App: "team-a", Index: 1, Image: "ghcr.io/app/worker"},
		{App: "shop", Index: 0, Image: "ghcr.io/app/shop"},
	}
	for _, tt := range []struct {
		name       string
		tags       map[int]string
		needed     []config.BuildRef
		neededTags map[string]map[int]string
		overrides  map[string]render.Image
		want       []render.Image
	}{
		{
			name:   "own and needed tags in entry order",
			tags:   map[int]string{0: "ksync-000000000001"},
			needed: needed,
			neededTags: map[string]map[int]string{
				"team-a": {0: "ksync-000000000002", 1: "ksync-000000000003"},
				"shop":   {0: "ksync-000000000004"},
			},
			want: []render.Image{
				{Name: "ghcr.io/app/api-b", NewTag: "ksync-000000000001"},
				{Name: "ghcr.io/app/seed", NewTag: "ksync-000000000002"},
				{Name: "ghcr.io/app/worker", NewTag: "ksync-000000000003"},
				{Name: "ghcr.io/app/shop", NewTag: "ksync-000000000004"},
			},
		},
		{
			name:       "overrides win over own and needed tags",
			tags:       map[int]string{0: "ksync-000000000001"},
			needed:     needed,
			neededTags: map[string]map[int]string{"team-a": {0: "ksync-000000000002"}},
			overrides: map[string]render.Image{
				"ghcr.io/app/api-b": {Name: "ghcr.io/app/api-b", NewTag: "supplied"},
				"ghcr.io/app/seed":  {Name: "ghcr.io/app/seed", NewName: "ghcr.io/app/seed-copy", Digest: "sha256:0123456789012345678901234567890123456789012345678901234567890123"},
			},
			want: []render.Image{
				{Name: "ghcr.io/app/api-b", NewTag: "supplied"},
				{Name: "ghcr.io/app/seed", NewName: "ghcr.io/app/seed-copy", Digest: "sha256:0123456789012345678901234567890123456789012345678901234567890123"},
			},
		},
		{
			name:      "needed override without owner in run",
			needed:    needed,
			overrides: map[string]render.Image{"ghcr.io/app/seed": {Name: "ghcr.io/app/seed", NewTag: "supplied"}},
			want:      []render.Image{{Name: "ghcr.io/app/seed", NewTag: "supplied"}},
		},
		{
			name:   "missing tags keep manifest refs",
			needed: needed,
		},
		{
			name:       "missing entry does not use another entry tag",
			needed:     needed,
			neededTags: map[string]map[int]string{"team-a": {1: "ksync-000000000003"}},
			want:       []render.Image{{Name: "ghcr.io/app/worker", NewTag: "ksync-000000000003"}},
		},
		{
			name:       "no needed builds ignore other apps tags and overrides",
			tags:       map[int]string{0: "ksync-000000000001"},
			neededTags: map[string]map[int]string{"team-a": {0: "ksync-000000000002"}},
			overrides:  map[string]render.Image{"ghcr.io/app/seed": {Name: "ghcr.io/app/seed", NewTag: "supplied"}},
			want:       []render.Image{{Name: "ghcr.io/app/api-b", NewTag: "ksync-000000000001"}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := syncImages(app, tt.tags, tt.needed, tt.neededTags, tt.overrides)
			if !slices.Equal(got, tt.want) {
				t.Errorf("syncImages = %+v, want %+v", got, tt.want)
			}
		})
	}
}
