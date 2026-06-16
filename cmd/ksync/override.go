package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/go-logr/logr"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
)

// overrideEnv is the environment variable holding image overrides: whitespace-
// separated IMAGE=REF tokens (newlines welcome, so a wrapper script can emit one
// per line). Each names a build entry's image and the pre-built ref to deploy in
// place of building it. Flags (--image) take precedence per image.
const overrideEnv = "KSYNC_IMAGE_OVERRIDES"

// stringSlice is a repeatable string flag (each --image appends one value).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// imageOverrides resolves the image overrides for a run from the KSYNC_IMAGE_OVERRIDES
// env var and the repeatable --image flags (flags win per image), returning a map
// keyed by the build entry's Image. An override naming an image that no app builds
// is dropped with a logged note rather than failing the run — so a wrapper can
// supply its full resolved image set without tracking which images ksync builds
// (the surplus is simply unused). See ADR 20260616-image-override.
func imageOverrides(cfg *config.Config, flagVals []string, log logr.Logger) (map[string]render.Image, error) {
	built := make(map[string]bool)
	for _, a := range cfg.Apps {
		for _, b := range a.Build {
			built[b.Image] = true
		}
	}

	// Env first, then flags on top, so a flag overrides the env for the same image.
	raw := make(map[string]string)
	if err := collectOverrides(raw, strings.Fields(os.Getenv(overrideEnv)), overrideEnv); err != nil {
		return nil, err
	}
	if err := collectOverrides(raw, flagVals, "--image"); err != nil {
		return nil, err
	}

	out := make(map[string]render.Image, len(raw))
	var unknown []string
	for image, ref := range raw {
		if !built[image] {
			unknown = append(unknown, image)
			continue
		}
		img, err := parseOverrideRef(image, ref)
		if err != nil {
			return nil, err
		}
		out[image] = img
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		log.Info("ignoring image overrides with no matching build entry", "images", strings.Join(unknown, ", "))
	}
	return out, nil
}

// collectOverrides parses IMAGE=REF tokens into dst (later tokens win). source
// names the origin (the env var or the flag) for error messages.
func collectOverrides(dst map[string]string, tokens []string, source string) error {
	for _, tok := range tokens {
		image, ref, ok := strings.Cut(tok, "=")
		if !ok || image == "" || ref == "" {
			return fmt.Errorf("%s: invalid override %q (want IMAGE=REF)", source, tok)
		}
		dst[image] = ref
	}
	return nil
}

// parseOverrideRef turns a supplied ref into the kustomize image replacement for
// the declared image. ref may be a bare tag (api stays the same image), a full
// ref (name:tag or name@digest), a leading-colon tag (:tag), or a leading-@
// digest (@sha256:…). A name part differing from image redirects the registry/
// repo (NewName); otherwise only the tag/digest changes.
func parseOverrideRef(image, ref string) (render.Image, error) {
	img := render.Image{Name: image}
	var name string // the ref's name part, if any (drives NewName)
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		// name@digest (name may be empty for the same-image case "@sha256:…").
		img.Digest = ref[at+1:]
		name = ref[:at]
		if img.Digest == "" {
			return render.Image{}, fmt.Errorf("override %q for image %s: empty digest after @", ref, image)
		}
	} else if tag, rest, ok := splitTag(ref); ok {
		img.NewTag = tag
		name = rest
	} else if !strings.ContainsRune(ref, '/') {
		// No tag, no digest, no registry/repo separator: a bare tag.
		img.NewTag = ref
		name = ""
	} else {
		return render.Image{}, fmt.Errorf("override %q for image %s: specify a tag (:tag) or digest (@sha256:…)", ref, image)
	}
	if name != "" && name != image {
		img.NewName = name
	}
	if img.NewTag == "" && img.Digest == "" {
		return render.Image{}, fmt.Errorf("override %q for image %s is empty", ref, image)
	}
	return img, nil
}

// splitTag separates a ref's tag from its name, recognizing a tag only in the
// last path segment (a colon earlier is a registry port). Returns the tag, the
// name without it, and whether a tag was found.
func splitTag(ref string) (tag, name string, ok bool) {
	slash := strings.LastIndexByte(ref, '/')
	colon := strings.LastIndexByte(ref, ':')
	if colon <= slash {
		return "", "", false
	}
	return ref[colon+1:], ref[:colon], true
}
