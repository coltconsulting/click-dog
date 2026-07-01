package main

import (
	"fmt"
	"io"
)

const deployUsage = `click-dog deploy — install, manage, and inspect a click-dog deployment

Usage:
  click-dog deploy <subcommand> [flags]

Subcommands:
  kubernetes  Generate Kubernetes manifests
  docker      Generate Docker Compose files
  status      Report installed version, systemd state, and health endpoint

Run "click-dog deploy <subcommand> --help" for subcommand flags.
`

func runDeploy(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(out, deployUsage)
		return 0
	}

	switch args[0] {
	case "-h", "--help", "help":
		_, _ = fmt.Fprint(out, deployUsage)
		return 0
	case "status":
		return runDeployStatus(args[1:], out, errOut)
	case "kubernetes":
		return runDeployKubernetes(args[1:], out, errOut)
	case "docker":
		return runDeployDocker(args[1:], out, errOut)
	}

	_, _ = fmt.Fprintf(errOut, "click-dog deploy: unknown subcommand %q\n\n", args[0])
	_, _ = fmt.Fprint(errOut, deployUsage)
	return 2
}
