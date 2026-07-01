package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// clickDogImagePinRE captures the tag of any click-dog image reference.
var clickDogImagePinRE = regexp.MustCompile(`ghcr\.io/coltconsulting/click-dog:([^\s"']+)`)

// imagePinSurfaces are the files that pin or render the click-dog container
// image: the checked-in sample manifest + docs example (shipped verbatim), the
// install.sh generators (the `${VERSION}` substitutions it runs/writes), and the
// `click-dog deploy` Go templates (`{{ .ImageTag }}`, filled by imageTag()).
var imagePinSurfaces = []string{
	"deploy/kubernetes/deployment.yaml",
	"docs/install.md",
	"deploy/install.sh",
	"deploy/templates/docker/docker-compose.yml.tmpl",
	"internal/deploy/template/k8s/deployment.yaml.tmpl",
	"internal/deploy/template/docker/docker-compose.yml.tmpl",
}

// TestImagePins_UseBareVersionScheme guards every image-pinning surface against
// the `:v…` scheme. GoReleaser publishes the image under the BARE {{ .Version }}
// — `.goreleaser.yaml` dockers tags use `{{ .Version }}` (e.g. `26.04.6`), and
// `{{ .Version }}` is the git tag with its leading `v` stripped. A `v`-prefixed
// pin therefore references a tag that does not exist on GHCR and fails with
// ErrImagePull (#232) — for the static samples AND for what the install.sh /
// `click-dog deploy` generators emit.
//
// A tag may legitimately be a bare version (`26.04.6`), a shell/Go substitution
// (`${VERSION}`, `{{ .ImageTag }}`), or a floating tag (`latest`) — none of
// which start with `v`. So the rule is simply: the tag must not start with `v`.
func TestImagePins_UseBareVersionScheme(t *testing.T) {
	for _, f := range imagePinSurfaces {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("reading %s: %v", f, err)
		}

		matches := clickDogImagePinRE.FindAllStringSubmatch(string(data), -1)
		if len(matches) == 0 {
			t.Errorf("%s: no ghcr.io/coltconsulting/click-dog: image reference found — "+
				"did it move or change format? This guard must keep covering it", f)
			continue
		}

		for _, m := range matches {
			tag := m[1]
			if strings.HasPrefix(tag, "v") {
				t.Errorf("%s: image pin %q uses the v-prefixed scheme; GoReleaser publishes the "+
					"bare {{ .Version }} (e.g. 26.04.6), so :v… is not a real GHCR tag (ErrImagePull, #232)", f, tag)
			}
		}
	}
}

// TestImageTag_BareScheme guards the Go generator's tag builder: imageTag() must
// produce a bare tag regardless of whether the operator supplied a leading `v`,
// since the {{ .ImageTag }} it fills renders the pin in every generated manifest.
func TestImageTag_BareScheme(t *testing.T) {
	for _, in := range []string{"26.05.1", "v26.05.1", "  v26.05.1  ", "26.06.2-beta"} {
		got := imageTag(in)
		if strings.HasPrefix(got, "v") {
			t.Errorf("imageTag(%q) = %q; want a bare tag (no leading v) to match GoReleaser {{ .Version }}", in, got)
		}
	}
	if got := imageTag("v26.05.1"); got != "26.05.1" {
		t.Errorf("imageTag(%q) = %q; want %q", "v26.05.1", got, "26.05.1")
	}
}
