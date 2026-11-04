package main

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/motoki317/ksync/internal/config"
	"github.com/motoki317/ksync/internal/render"
	"github.com/motoki317/ksync/internal/ui"
)

func TestAddNeededLiveTags(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{
		{Name: "team-a", Namespace: "builds", Build: []config.Build{{Image: "ghcr.io/app/seed"}, {Image: "ghcr.io/app/worker"}}},
		{Name: "api-b", Needs: []string{"team-a"}},
		{Name: "shop", Needs: []string{"team-a"}},
		{Name: "proxy", Needs: []string{"api-b"}},
		{Name: "plain"},
	}}
	liveImage := func(repo, tag string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{"image": repo + ":" + tag},
		}}
	}
	const tag = "ksync-0123456789ab"
	seed := render.Image{Name: "ghcr.io/app/seed", NewTag: tag}
	worker := render.Image{Name: "ghcr.io/app/worker", NewTag: tag}
	for _, tt := range []struct {
		name      string
		names     []string
		live      []*unstructured.Unstructured
		overrides map[string]render.Image
		want      map[string]render.Image
		wantReads int
	}{
		{
			name: "excluded owner supplies consistent live tags", names: []string{"api-b"},
			live:      []*unstructured.Unstructured{liveImage(seed.Name, tag), liveImage(worker.Name, tag), liveImage(seed.Name, tag)},
			want:      map[string]render.Image{seed.Name: seed, worker.Name: worker},
			wantReads: 1,
		},
		{
			name: "missing live images keep manifest refs", names: []string{"api-b"},
			wantReads: 1,
		},
		{
			name: "disagreeing and non-dev tags keep manifest refs", names: []string{"api-b"},
			live: []*unstructured.Unstructured{
				liveImage(seed.Name, tag), liveImage(seed.Name, "ksync-ffffffffffff"), liveImage(worker.Name, "stable"),
			},
			wantReads: 1,
		},
		{
			name: "explicit override wins", names: []string{"api-b"},
			overrides: map[string]render.Image{seed.Name: {Name: seed.Name, NewName: "ghcr.io/app/seed-copy", Digest: "sha256:" + zeroDigest}},
			want:      map[string]render.Image{seed.Name: {Name: seed.Name, NewName: "ghcr.io/app/seed-copy", Digest: "sha256:" + zeroDigest}},
			wantReads: 1,
		},
		{
			name: "all overrides skip lookup", names: []string{"api-b"},
			overrides: map[string]render.Image{seed.Name: seed, worker.Name: worker},
			want:      map[string]render.Image{seed.Name: seed, worker.Name: worker},
		},
		{
			name: "live tags fill only images without explicit overrides", names: []string{"api-b"},
			live:      []*unstructured.Unstructured{liveImage(seed.Name, tag), liveImage(worker.Name, tag)},
			overrides: map[string]render.Image{seed.Name: {Name: seed.Name, NewTag: "supplied"}},
			want:      map[string]render.Image{seed.Name: {Name: seed.Name, NewTag: "supplied"}, worker.Name: worker},
			wantReads: 1,
		},
		{
			name: "transitive owner is not read", names: []string{"proxy"},
			live: []*unstructured.Unstructured{liveImage(seed.Name, tag), liveImage(worker.Name, tag)},
		},
		{name: "unrelated app does no lookup", names: []string{"plain"}},
		{
			name: "shared owner is read once for several consumers", names: []string{"api-b", "shop"},
			live:      []*unstructured.Unstructured{liveImage(seed.Name, tag), liveImage(worker.Name, tag)},
			want:      map[string]render.Image{seed.Name: seed, worker.Name: worker},
			wantReads: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			apps, err := cfg.Select(tt.names, nil)
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			read := func(app, namespace string, resources []*unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
				reads++
				if app != "team-a" || namespace != "builds" || resources != nil {
					t.Errorf("live read = (%q, %q, %v), want (team-a, builds, nil)", app, namespace, resources)
				}
				return tt.live, nil
			}
			got := make(map[string]render.Image)
			maps.Copy(got, tt.overrides)
			err = addNeededLiveTags(cfg, apps, got, read)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, tt.want) {
				t.Errorf("images = %+v, want %+v", got, tt.want)
			}
			if reads != tt.wantReads {
				t.Errorf("live reads = %d, want %d", reads, tt.wantReads)
			}
		})
	}
}

func TestAddNeededLiveTags_MultipleOwners(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{
		{Name: "team-a", Namespace: "builds", Build: []config.Build{{Image: "ghcr.io/app/seed"}}},
		{Name: "api-b", Namespace: "jobs", Build: []config.Build{{Image: "ghcr.io/app/worker"}}},
		{Name: "shop", Needs: []string{"team-a", "api-b"}},
	}}
	for _, tt := range []struct {
		names     []string
		wantReads []string
		want      map[string]render.Image
	}{
		{
			names: []string{"shop"}, wantReads: []string{"team-a/builds", "api-b/jobs"},
			want: map[string]render.Image{
				"ghcr.io/app/seed":   {Name: "ghcr.io/app/seed", NewTag: "ksync-0123456789ab"},
				"ghcr.io/app/worker": {Name: "ghcr.io/app/worker", NewTag: "ksync-ffffffffffff"},
			},
		},
		{
			names: []string{"shop", "team-a"}, wantReads: []string{"api-b/jobs"},
			want: map[string]render.Image{"ghcr.io/app/worker": {Name: "ghcr.io/app/worker", NewTag: "ksync-ffffffffffff"}},
		},
	} {
		t.Run(strings.Join(tt.names, ","), func(t *testing.T) {
			apps, err := cfg.Select(tt.names, nil)
			if err != nil {
				t.Fatal(err)
			}
			var reads []string
			read := func(app, namespace string, resources []*unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
				reads = append(reads, app+"/"+namespace)
				if resources != nil {
					t.Errorf("live read target = %v, want nil", resources)
				}
				ref := "ghcr.io/app/seed:ksync-0123456789ab"
				if app == "api-b" {
					ref = "ghcr.io/app/worker:ksync-ffffffffffff"
				}
				return []*unstructured.Unstructured{{Object: map[string]any{"spec": map[string]any{"image": ref}}}}, nil
			}
			got := make(map[string]render.Image)
			err = addNeededLiveTags(cfg, apps, got, read)
			if err != nil || !maps.Equal(got, tt.want) || !reflect.DeepEqual(reads, tt.wantReads) {
				t.Errorf("images = %+v, reads = %v, err = %v; want %+v, %v, no error", got, reads, err, tt.want, tt.wantReads)
			}
		})
	}
}

func TestAddNeededLiveTags_ReadError(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{
		{Name: "team-a", Build: []config.Build{{Image: "ghcr.io/app/seed"}}},
		{Name: "api-b", Needs: []string{"team-a"}},
	}}
	wantErr := errors.New("cache unavailable")
	read := func(string, string, []*unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
		return nil, wantErr
	}
	err := addNeededLiveTags(cfg, cfg.Apps[1:], make(map[string]render.Image), read)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "team-a") {
		t.Errorf("error = %v, want owner and wrapped %v", err, wantErr)
	}
}

func TestNeededImageNotes(t *testing.T) {
	const seed = "ghcr.io/app/seed"
	const other = "ghcr.io/app/other"
	cfg := &config.Config{Apps: []config.App{
		{Name: "team-a", Build: []config.Build{{Image: seed}, {Image: other}}},
		{Name: "api-b", Needs: []string{"team-a"}},
		{Name: "shop", Needs: []string{"api-b"}},
	}}
	for _, tt := range []struct {
		name      string
		names     []string
		overrides map[string]render.Image
		patch     string
		want      string
	}{
		{name: "only referenced image", names: []string{"api-b"}, want: seed},
		{name: "patch changes referenced image", names: []string{"api-b"}, patch: other, want: other},
		{name: "patch removes needed image", names: []string{"api-b"}, patch: "redis"},
		{name: "supplied ref needs no note", names: []string{"api-b"}, overrides: map[string]render.Image{seed: {Name: seed, NewTag: "supplied"}}},
		{name: "selected owner needs no note", names: []string{"api-b", "team-a"}},
		{name: "transitive need gets no note", names: []string{"shop"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			apps, err := cfg.Select(tt.names, nil)
			if err != nil {
				t.Fatal(err)
			}
			res := renderNeededImage(t, seed+":manifest")
			if tt.patch != "" {
				if err := res.ApplyPatches([]config.Patch{{
					Target: config.PatchTarget{Kind: "Pod", Name: "api-b"},
					Patch:  `[{"op":"replace","path":"/spec/containers/0/image","value":"` + tt.patch + `:manifest"}]`,
				}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			var notes bytes.Buffer
			ui.WriteLine(ui.SectionLog, &notes, "preceding log\n")
			notes.Reset()
			note := neededImageNotes(cfg, apps, tt.overrides, &notes)
			if note != nil {
				note(apps[0].Name, res.ReferencedRepos())
				note(apps[0].Name, res.ReferencedRepos())
			}
			want := ""
			if tt.want != "" {
				want = "note: api-b deploys the manifest ref of " + tt.want + ", because team-a is not selected and runs no single ksync dev tag of it. Select team-a too, or pass --image " + tt.want + "=REF.\n"
			}
			if notes.String() != want {
				t.Errorf("notes = %q, want %q", notes.String(), want)
			}
		})
	}
}

func renderNeededImage(t *testing.T, ref string) *render.Result {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"kustomization.yaml": "resources: [pod.yaml]\n",
		"pod.yaml":           "apiVersion: v1\nkind: Pod\nmetadata:\n  name: api-b\nspec:\n  containers:\n    - name: seed\n      image: " + ref + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := render.New(render.Options{}).Render(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestNeededImageNotes_ConsumersAndLaterRefs(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{
		{Name: "team-a", Build: []config.Build{{Image: "seed"}, {Image: "worker"}}},
		{Name: "api-b", Needs: []string{"team-a"}},
		{Name: "shop", Needs: []string{"team-a"}},
	}}
	var notes bytes.Buffer
	note := neededImageNotes(cfg, cfg.Apps[1:], nil, &notes)
	var wg sync.WaitGroup
	for _, name := range []string{"api-b", "shop"} {
		wg.Go(func() {
			note(name, map[string]bool{"seed": true})
			note(name, map[string]bool{"seed": true})
			note(name, map[string]bool{"worker": true})
		})
	}
	wg.Wait()
	for _, name := range []string{"api-b", "shop"} {
		for _, image := range []string{"seed", "worker"} {
			want := "note: " + name + " deploys the manifest ref of " + image + ","
			if got := strings.Count(notes.String(), want); got != 1 {
				t.Errorf("count of %q = %d, want 1 in %q", want, got, notes.String())
			}
		}
	}
}

func TestNeededImageNotes_CommittedLogLine(t *testing.T) {
	cfg := &config.Config{Apps: []config.App{
		{Name: "team-a", Build: []config.Build{{Image: "seed"}}},
		{Name: "api-b", Needs: []string{"team-a"}},
	}}
	var notes bytes.Buffer
	ui.WriteLine(ui.SectionPipeline, &notes, "build output\n")
	notes.Reset()
	note := neededImageNotes(cfg, cfg.Apps[1:], nil, &notes)
	note("api-b", map[string]bool{"seed": true})
	ui.WriteLine(ui.SectionLog, &notes, "watching\n")
	want := "\nnote: api-b deploys the manifest ref of seed, because team-a is not selected and runs no single ksync dev tag of it. Select team-a too, or pass --image seed=REF.\nwatching\n"
	if notes.String() != want {
		t.Errorf("committed note = %q, want %q", notes.String(), want)
	}
}
