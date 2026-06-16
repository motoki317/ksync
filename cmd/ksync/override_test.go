package main

import (
	"testing"

	"github.com/go-logr/logr"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
)

func TestParseOverrideRef(t *testing.T) {
	const image = "ghcr.io/org/api-b"
	cases := []struct {
		name string
		ref  string
		want render.Image
	}{
		{"bare tag", "dev-xyz", render.Image{Name: image, NewTag: "dev-xyz"}},
		{"leading-colon tag", ":dev-xyz", render.Image{Name: image, NewTag: "dev-xyz"}},
		{"full ref same name", image + ":dev-xyz", render.Image{Name: image, NewTag: "dev-xyz"}},
		{"digest same name", image + "@sha256:abc", render.Image{Name: image, Digest: "sha256:abc"}},
		{"leading-@ digest", "@sha256:abc", render.Image{Name: image, Digest: "sha256:abc"}},
		{"redirect name+tag", "other.io/x/api-b:tag", render.Image{Name: image, NewName: "other.io/x/api-b", NewTag: "tag"}},
		// The colon in a registry port is not a tag separator.
		{"registry port", "localhost:5000/api-b:tag", render.Image{Name: image, NewName: "localhost:5000/api-b", NewTag: "tag"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseOverrideRef(image, c.ref)
			if err != nil {
				t.Fatalf("parseOverrideRef(%q): %v", c.ref, err)
			}
			if got != c.want {
				t.Errorf("parseOverrideRef(%q) = %+v, want %+v", c.ref, got, c.want)
			}
		})
	}
}

func TestParseOverrideRef_Errors(t *testing.T) {
	const image = "ghcr.io/org/api-b"
	for _, ref := range []string{
		"ghcr.io/org/api-b",  // name with no tag and no digest
		"ghcr.io/org/api-b@", // empty digest
		"",                   // empty
	} {
		if _, err := parseOverrideRef(image, ref); err == nil {
			t.Errorf("parseOverrideRef(%q) = nil error, want a failure", ref)
		}
	}
}

func TestImageOverrides_EnvAndFlagsMerge(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{{
		Name: "duo",
		Build: []config.Build{
			{Image: "ghcr.io/org/api-b"},
			{Image: "ghcr.io/org/ui-b"},
		},
	}}}
	// Env supplies api-b; the flag supplies ui-b and also overrides api-b — the
	// flag must win for the shared image.
	t.Setenv(overrideEnv, "ghcr.io/org/api-b=env-tag")
	flags := []string{"ghcr.io/org/api-b=flag-tag", "ghcr.io/org/ui-b=ui-tag"}

	got, err := imageOverrides(cfg, flags, logr.Discard())
	if err != nil {
		t.Fatalf("imageOverrides: %v", err)
	}
	if got["ghcr.io/org/api-b"].NewTag != "flag-tag" {
		t.Errorf("api-b = %+v, want flag-tag (flag overrides env)", got["ghcr.io/org/api-b"])
	}
	if got["ghcr.io/org/ui-b"].NewTag != "ui-tag" {
		t.Errorf("ui-b = %+v, want ui-tag", got["ghcr.io/org/ui-b"])
	}
}

// An override naming an image no app builds is dropped (not an error), so a
// wrapper can supply a superset of refs without tracking ksync's build list.
func TestImageOverrides_DropsUnknownImage(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{{
		Name:  "duo",
		Build: []config.Build{{Image: "ghcr.io/org/api-b"}},
	}}}
	got, err := imageOverrides(cfg, []string{
		"ghcr.io/org/api-b=tag",
		"ghcr.io/org/not-built=tag",
	}, logr.Discard())
	if err != nil {
		t.Fatalf("imageOverrides: %v", err)
	}
	if _, ok := got["ghcr.io/org/not-built"]; ok {
		t.Error("override for an unbuilt image should be dropped, not applied")
	}
	if _, ok := got["ghcr.io/org/api-b"]; !ok {
		t.Error("override for a built image should be kept")
	}
}

func TestImageOverrides_RejectsMalformed(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{{Name: "duo", Build: []config.Build{{Image: "img"}}}}}
	if _, err := imageOverrides(cfg, []string{"no-equals-sign"}, logr.Discard()); err == nil {
		t.Error("imageOverrides accepted a token with no '='; want an error")
	}
}
