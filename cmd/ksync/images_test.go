package main

import (
	"reflect"
	"testing"
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

// zeroDigest is a syntactically valid 64-hex sha256 digest for the digest-ref case.
const zeroDigest = "0000000000000000000000000000000000000000000000000000000000000000"
