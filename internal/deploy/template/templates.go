// Package template embeds deploy-time text/template assets (systemd unit,
// click-dog.yaml, kubernetes manifests, docker compose) consumed by the
// click-dog deploy subcommands.
package template

import (
	"bytes"
	"embed"
	pathpkg "path"
	texttemplate "text/template"
)

//go:embed k8s/*.tmpl docker/*.tmpl
var FS embed.FS

// Render executes an embedded template by path, for example
// "k8s/deployment.yaml.tmpl".
func Render(path string, data any) ([]byte, error) {
	tmpl, err := texttemplate.New("").Option("missingkey=error").ParseFS(FS, path)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, pathpkg.Base(path), data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
