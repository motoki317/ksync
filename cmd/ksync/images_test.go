package main

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/motoki317/ksync/internal/config"
)

func TestNormalizeRef(t *testing.T) {
	cases := []struct {
		ref        string
		repo, full string
		ok         bool
	}{
		// Short docker.io refs gain the implied library/ and registry, matching
		// what containerd stores.
		{"redis:7", "docker.io/library/redis", "docker.io/library/redis:7", true},
		{"alpine", "docker.io/library/alpine", "docker.io/library/alpine:latest", true},
		{"localstack/localstack:4.0", "docker.io/localstack/localstack", "docker.io/localstack/localstack:4.0", true},
		// A fully-qualified ref is left as-is apart from an implied :latest.
		{"ghcr.io/team-a/api:v1", "ghcr.io/team-a/api", "ghcr.io/team-a/api:v1", true},
		{"docker.elastic.co/elasticsearch/elasticsearch:8.13.0", "docker.elastic.co/elasticsearch/elasticsearch", "docker.elastic.co/elasticsearch/elasticsearch:8.13.0", true},
		// Digest refs keep the digest; no :latest is added.
		{"ghcr.io/team-a/api@sha256:" + zeroDigest, "ghcr.io/team-a/api", "ghcr.io/team-a/api@sha256:" + zeroDigest, true},
		// Surrounding whitespace (e.g. a trailing newline from a list) is trimmed.
		{"  redis:7\n", "docker.io/library/redis", "docker.io/library/redis:7", true},
		{"", "", "", false},
		{"NOT AN IMAGE", "", "", false},
	}
	for _, c := range cases {
		repo, full, ok := normalizeRef(c.ref)
		if ok != c.ok || repo != c.repo || full != c.full {
			t.Errorf("normalizeRef(%q) = (%q, %q, %v), want (%q, %q, %v)", c.ref, repo, full, ok, c.repo, c.full, c.ok)
		}
	}
}

func TestImageSet(t *testing.T) {
	built := map[string]bool{"ghcr.io/team-a/api": true}
	s := newImageSet(built)
	s.add("redis:7")                   // short -> canonical
	s.add("redis:7")                   // duplicate collapses
	s.add("ghcr.io/team-a/api:devtag") // built repo dropped whatever the tag
	s.add("alpine")                    // gains :latest
	s.add("")                          // unparseable ignored
	s.add("@@@")                       // unparseable ignored

	got := s.sorted()
	want := []string{"docker.io/library/alpine:latest", "docker.io/library/redis:7"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sorted() = %v, want %v", got, want)
	}
}

func TestAppImageRefs(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{
		{Name: "team-a", Build: []config.Build{{Image: "seed"}, {Image: "ghcr.io/app/worker"}}},
		{Name: "api-b", Needs: []string{"team-a"}, Build: []config.Build{{Image: "ghcr.io/app/api-b"}}},
		{Name: "shop", Needs: []string{"api-b"}},
		{Name: "plain"},
	}}
	refs := []string{
		"seed:set-by-stack",
		"docker.io/library/seed@sha256:" + zeroDigest,
		"ghcr.io/app/worker:set-by-stack",
		"ghcr.io/app/api-b:set-by-stack",
		"redis:7",
	}
	for _, tt := range []struct {
		name string
		app  string
		want []string
	}{
		{
			name: "owner excludes own builds", app: "team-a",
			want: []string{"docker.io/library/redis:7", "ghcr.io/app/api-b:set-by-stack"},
		},
		{
			name: "consumer excludes own and needed builds with owner unselected", app: "api-b",
			want: []string{"docker.io/library/redis:7"},
		},
		{
			name: "transitive owner is not excluded", app: "shop",
			want: []string{"docker.io/library/redis:7", "docker.io/library/seed:set-by-stack", "docker.io/library/seed@sha256:" + zeroDigest, "ghcr.io/app/worker:set-by-stack"},
		},
		{
			name: "unrelated app retains built repositories", app: "plain",
			want: []string{"docker.io/library/redis:7", "docker.io/library/seed:set-by-stack", "docker.io/library/seed@sha256:" + zeroDigest, "ghcr.io/app/api-b:set-by-stack", "ghcr.io/app/worker:set-by-stack"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			apps, err := cfg.Select([]string{tt.app}, nil)
			if err != nil {
				t.Fatal(err)
			}
			app := apps[0]
			if got := appImageRefs(cfg, app, refs); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("images = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAddPodImages_BuiltRepos(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{
		{Name: "team-a", Build: []config.Build{{Image: "seed"}}},
		{Name: "api-b", Build: []config.Build{{Image: "ghcr.io/app/api-b"}}},
	}}
	images := newImageSet(nil)
	images.add("seed:registry-tag")
	addPodImages(images, cfg, []corev1.Pod{{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Image: "docker.io/library/seed:ksync-012345678901"}},
		Containers: []corev1.Container{
			{Image: "ghcr.io/app/api-b:ksync-012345678901"},
			{Image: "redis:7"},
		},
		EphemeralContainers: []corev1.EphemeralContainer{
			{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Image: "seed@sha256:" + zeroDigest}},
			{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Image: "alpine"}},
		},
	}}})
	want := []string{"docker.io/library/alpine:latest", "docker.io/library/redis:7", "docker.io/library/seed:registry-tag"}
	if got := images.sorted(); !reflect.DeepEqual(got, want) {
		t.Errorf("live images = %v, want %v", got, want)
	}
}

// zeroDigest is a syntactically valid 64-hex sha256 digest for the digest-ref case.
const zeroDigest = "0000000000000000000000000000000000000000000000000000000000000000"
