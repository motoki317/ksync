package render

import (
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// devTagPattern matches a ksync content-addressed dev tag (ksync-<12 hex>, see
// internal/build). Only these tags are carried forward in a diff — any other tag
// is a real ref whose diff must show.
var devTagPattern = regexp.MustCompile(`^ksync-[0-9a-f]{12}$`)

// CarryForwardBuildTags suppresses the per-build dev-tag churn in `ksync diff`:
// for each build image repo, when the live cluster currently runs it at a ksync
// dev tag (ksync-<12hex>), that ref is injected into the rendered target through
// the same field-spec-aware path SetImages uses — so the target's image fields,
// including CRD-embedded ones declared via the kustomization's `configurations:`,
// match live and drop out of the diff (and pick up the identical
// Always→IfNotPresent fix). A repo with no managed live image, an inconsistent
// set, or a non-ksync tag is left untouched so its real diff shows. It returns
// the repos it carried forward (in `repos` order), for the caller's note.
//
// `diff` does not build, so the carried tag is the *current* deployed one: an
// edited-but-unsynced source change does not appear as an image diff (diff
// cannot know the new content hash without building).
func (res *Result) CarryForwardBuildTags(live []*unstructured.Unstructured, repos []string) ([]string, error) {
	tags := liveDevTags(live, repos)
	if len(tags) == 0 {
		return nil, nil
	}
	images := make([]Image, 0, len(tags))
	carried := make([]string, 0, len(tags))
	for _, repo := range repos {
		if tag, ok := tags[repo]; ok {
			images = append(images, Image{Name: repo, NewTag: tag})
			carried = append(carried, repo)
		}
	}
	if err := res.SetImages(images); err != nil {
		return nil, err
	}
	return carried, nil
}

// ReferencedRepos returns the set of image repos referenced anywhere in the
// rendered objects (deep-walked, so a CRD-embedded image counts). `ksync diff`
// uses it to drop a `build:` entry whose image the manifests no longer deploy:
// such an orphaned entry would otherwise produce a phantom "shown at source ref"
// note and wrongly keep the app out of "in sync", though a sync would change no
// image. Pure; the repo extraction matches SetImages/liveDevTags.
func (res *Result) ReferencedRepos() map[string]bool {
	repos := map[string]bool{}
	for _, o := range res.Objects {
		for _, ref := range collectImageRefs(o.Object) {
			repos[imageRepo(ref)] = true
		}
	}
	return repos
}

// liveDevTags returns, for each requested repo, the ksync dev tag the live
// objects run it at — but only when every occurrence agrees and the tag matches
// ksync-<12hex>. A repo absent, inconsistent, or seen at any non-ksync ref is
// omitted, so the caller falls open to showing the real diff. It deep-walks the
// objects for image references (any `image` string and the `image.reference` OCI
// volume path), so a CRD's non-standard image path is covered without the field
// specs. Pure, so the rule is unit-tested without a cluster.
func liveDevTags(objs []*unstructured.Unstructured, repos []string) map[string]string {
	want := make(map[string]bool, len(repos))
	for _, r := range repos {
		want[r] = true
	}
	found := map[string]string{}  // repo -> dev tag (first seen)
	conflict := map[string]bool{} // repo -> saw a disagreeing or non-ksync ref
	for _, o := range objs {
		for _, ref := range collectImageRefs(o.Object) {
			repo := imageRepo(ref)
			if !want[repo] || conflict[repo] {
				continue
			}
			tag := imageTag(ref)
			if !devTagPattern.MatchString(tag) {
				conflict[repo] = true // a non-ksync ref disqualifies the repo
				continue
			}
			if prev, ok := found[repo]; ok {
				if prev != tag {
					conflict[repo] = true
				}
			} else {
				found[repo] = tag
			}
		}
	}
	out := make(map[string]string, len(found))
	for repo, tag := range found {
		if !conflict[repo] {
			out[repo] = tag
		}
	}
	return out
}

// collectImageRefs deep-walks a decoded object for container image references:
// every string value at an `image` key, plus the `image.reference` string of an
// OCI volume source. Walking by key rather than by path makes it agnostic to
// where the image sits, so a CRD-embedded image is found too.
func collectImageRefs(v any) []string {
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if k == "image" {
					switch iv := val.(type) {
					case string:
						out = append(out, iv)
					case map[string]any:
						if ref, ok := iv["reference"].(string); ok {
							out = append(out, ref)
						}
					}
				}
				walk(val)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

// imageTag returns the tag portion of an image reference, or "" when it carries
// none (or only a digest). It mirrors imageRepo's port-vs-tag rule: a ':' starts
// the tag only when no '/' follows it.
func imageTag(ref string) string {
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		ref = ref[:at]
	}
	if c := strings.LastIndexByte(ref, ':'); c >= 0 && !strings.ContainsRune(ref[c:], '/') {
		return ref[c+1:]
	}
	return ""
}
