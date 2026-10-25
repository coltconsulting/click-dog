package main

import (
	"fmt"
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
	"docs/kubernetes.md",
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
		t.Run(f, func(t *testing.T) {
			if strings.HasPrefix(f, "docs/") {
				requireInternalDocs(t)
			}
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("reading %s: %v", f, err)
			}

			matches := clickDogImagePinRE.FindAllStringSubmatch(string(data), -1)
			if len(matches) == 0 {
				t.Errorf("%s: no ghcr.io/coltconsulting/click-dog: image reference found — "+
					"did it move or change format? This guard must keep covering it", f)
				return
			}

			for _, m := range matches {
				tag := m[1]
				if strings.HasPrefix(tag, "v") {
					t.Errorf("%s: image pin %q uses the v-prefixed scheme; GoReleaser publishes the "+
						"bare {{ .Version }} (e.g. 26.04.6), so :v… is not a real GHCR tag (ErrImagePull, #232)", f, tag)
				}
			}
		})
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

// TestDockerfile_UserMatchesKubernetesIdentity ties the image's account to the
// pod identity the shipped manifest sets. A numeric USER lets the kubelet verify
// runAsNonRoot without a pod runAsUser, and an account with that uid gives a pod
// that sets only runAsUser the matching group instead of group 0.
func TestDockerfile_UserMatchesKubernetesIdentity(t *testing.T) {
	pod, _ := podSecurityContexts(t, shippedDeployment)
	uid, gid := pod["runAsUser"], pod["runAsGroup"]
	dockerfile := readFile(t, "Dockerfile")

	users := regexp.MustCompile(`(?m)^USER\s+(\S+)\s*$`).FindAllStringSubmatch(dockerfile, -1)
	if len(users) == 0 {
		t.Fatal("Dockerfile has no USER")
	}
	if got, want := users[len(users)-1][1], fmt.Sprintf("%v:%v", uid, gid); got != want {
		t.Errorf("Dockerfile USER = %s, want %s (numeric, the manifest's runAsUser:runAsGroup)", got, want)
	}
	for _, want := range []string{
		fmt.Sprintf("addgroup -S -g %v clickdog", gid),
		fmt.Sprintf("adduser -S -u %v -G clickdog clickdog", uid),
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile missing %q: without an account for that uid, a pod that sets only runAsUser runs in group 0", want)
		}
	}
}
