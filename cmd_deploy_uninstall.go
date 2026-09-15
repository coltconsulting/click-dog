package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

const deployUninstallUsage = `click-dog deploy uninstall — remove a standard systemd installation

Usage:
  sudo click-dog deploy uninstall [--yes]

The command stops and disables click-dog, then removes its systemd unit,
installed binary and rollback binary, configuration, credentials, log
directory, and runtime user/group. Docker and Kubernetes deployments must be
removed with their orchestrator. ClickHouse users and configuration are not
changed. A runtime user or group is removed only when the installer recorded
that it created the same numeric identity.

Flags:
`

type deployUninstallPaths struct {
	unit      string
	binary    string
	configDir string
	logDir    string
	stateDir  string
	progress  string
	runtimeID string
}

var standardDeployUninstallPaths = deployUninstallPaths{
	unit:      "/etc/systemd/system/click-dog.service",
	binary:    "/usr/local/bin/click-dog",
	configDir: "/etc/click-dog",
	logDir:    "/var/log/click-dog",
	stateDir:  "/var/lib/click-dog-installer",
	progress:  "/var/lib/click-dog-uninstall-in-progress",
	runtimeID: "click-dog",
}

type deployUninstallDeps struct {
	euid        func() int
	in          io.Reader
	lstat       func(string) (os.FileInfo, error)
	readFile    func(string) ([]byte, error)
	writeFile   func(string, []byte, os.FileMode) error
	link        func(string, string) error
	remove      func(string) error
	removeAll   func(string) error
	lookupUser  func(string) (*user.User, error)
	lookupGroup func(string) (*user.Group, error)
	run         func(string, ...string) error
}

func defaultDeployUninstallDeps() deployUninstallDeps {
	return deployUninstallDeps{
		euid:        os.Geteuid,
		in:          os.Stdin,
		lstat:       os.Lstat,
		readFile:    os.ReadFile,
		writeFile:   os.WriteFile,
		link:        os.Link,
		remove:      os.Remove,
		removeAll:   os.RemoveAll,
		lookupUser:  user.Lookup,
		lookupGroup: user.LookupGroup,
		run: func(name string, args ...string) error {
			output, err := exec.Command(name, args...).CombinedOutput() //nolint:gosec // fixed commands and arguments only
			if err == nil {
				return nil
			}
			message := strings.TrimSpace(string(output))
			if message == "" {
				return err
			}
			return fmt.Errorf("%w: %s", err, message)
		},
	}
}

func deployUninstallPathExists(lstat func(string) (os.FileInfo, error), path string) (bool, error) {
	if _, err := lstat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func deployUninstallMarkerID(readFile func(string) ([]byte, error), path string) (string, bool, error) {
	data, err := readFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	id := strings.TrimSpace(string(data))
	parsed, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		return "", false, fmt.Errorf("invalid numeric identity %q", id)
	}
	return strconv.FormatUint(parsed, 10), true, nil
}

func deployUninstallUnknownUser(err error) bool {
	var unknown user.UnknownUserError
	return errors.As(err, &unknown)
}

func deployUninstallUnknownGroup(err error) bool {
	var unknown user.UnknownGroupError
	return errors.As(err, &unknown)
}

func deployUninstallSystemdUnitAbsent(err error, service string) bool {
	if err == nil {
		return false
	}
	// Do not accept "is masked": masking prevents future activation but does
	// not prove that an already-running unit was stopped.
	message := strings.ToLower(err.Error())
	service = strings.ToLower(service)
	for _, phrase := range []string{
		"unit " + service + " not loaded",
		"unit " + service + " does not exist",
		"unit file " + service + " does not exist",
	} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

func runDeployUninstall(args []string, out, errOut io.Writer) int {
	return runDeployUninstallWith(args, out, errOut, standardDeployUninstallPaths, defaultDeployUninstallDeps())
}

func runDeployUninstallWith(
	args []string,
	out, errOut io.Writer,
	paths deployUninstallPaths,
	deps deployUninstallDeps,
) int {
	fs := flag.NewFlagSet("deploy uninstall", flag.ContinueOnError)
	fs.SetOutput(errOut)
	yes := fs.Bool("yes", false, "Remove the installation without an interactive confirmation")
	fs.Usage = func() {
		_, _ = fmt.Fprint(errOut, deployUninstallUsage)
		printFlagDefaults(errOut, fs)
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(errOut, "click-dog deploy uninstall: unexpected argument %q\n\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if deps.euid() != 0 {
		_, _ = fmt.Fprintln(errOut, "Error: uninstall requires root; run 'sudo click-dog deploy uninstall'.")
		return 1
	}
	unitExists, err := deployUninstallPathExists(deps.lstat, paths.unit)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: inspect systemd unit: %v\n", err)
		return 1
	}
	progressExists, err := deployUninstallPathExists(deps.lstat, paths.progress)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: inspect uninstall progress marker: %v\n", err)
		return 1
	}
	recovery := paths.binary + ".uninstalling"
	recoveryExists, err := deployUninstallPathExists(deps.lstat, recovery)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: inspect uninstall recovery binary: %v\n", err)
		return 1
	}
	managedStateExists := unitExists || progressExists || recoveryExists
	for _, artifact := range []struct {
		label string
		path  string
	}{
		{label: "installed binary", path: paths.binary},
		{label: "rollback binary", path: paths.binary + ".prev"},
		{label: "configuration directory", path: paths.configDir},
		{label: "log directory", path: paths.logDir},
		{label: "installer state directory", path: paths.stateDir},
	} {
		exists, inspectErr := deployUninstallPathExists(deps.lstat, artifact.path)
		if inspectErr != nil {
			_, _ = fmt.Fprintf(errOut, "Error: inspect %s: %v\n", artifact.label, inspectErr)
			return 1
		}
		managedStateExists = managedStateExists || exists
	}
	if !managedStateExists {
		if *yes {
			_, _ = fmt.Fprintln(out, "Standard systemd installation state already absent; nothing to remove.")
			return 0
		}
		_, _ = fmt.Fprintf(errOut, "Error: no standard systemd installation found at %s.\n", paths.unit)
		_, _ = fmt.Fprintln(errOut, "Remove Docker or Kubernetes deployments with their orchestrator.")
		return 1
	}

	_, _ = fmt.Fprintln(out, "This permanently removes the standard Click-Dog systemd installation:")
	_, _ = fmt.Fprintf(out, "  Service: %s\n", paths.unit)
	_, _ = fmt.Fprintf(out, "  Binary:  %s (and .prev)\n", paths.binary)
	_, _ = fmt.Fprintf(out, "  Config:  %s (including credentials)\n", paths.configDir)
	_, _ = fmt.Fprintf(out, "  Logs:    %s\n", paths.logDir)
	_, _ = fmt.Fprintf(out, "  User:    %s (only if recorded as installer-created)\n", paths.runtimeID)
	_, _ = fmt.Fprintln(out, "  ClickHouse: unchanged (monitoring user and span-log config remain)")
	if progressExists || recoveryExists {
		_, _ = fmt.Fprintln(out, "  Mode:    resume an earlier incomplete uninstall")
	}

	if !*yes {
		_, _ = fmt.Fprint(out, "Continue? [y/N] ")
		answer, readErr := bufio.NewReader(deps.in).ReadString('\n')
		answer = strings.TrimSpace(answer)
		if readErr != nil && !errors.Is(readErr, io.EOF) ||
			!strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
			_, _ = fmt.Fprintln(out, "Aborted.")
			return 0
		}
	}

	// Keep a second hard link to the running executable until finalization.
	// Linux permits unlinking a running executable, so without this recovery
	// link a later failure could leave the documented retry command missing.
	binaryExists, err := deployUninstallPathExists(deps.lstat, paths.binary)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: inspect installed binary: %v\n", err)
		return 1
	}
	managedBinary := binaryExists || recoveryExists
	if binaryExists {
		if recoveryExists {
			if err := deps.remove(recovery); err != nil {
				_, _ = fmt.Fprintf(errOut, "Error: replace uninstall recovery binary: %v\n", err)
				return 1
			}
		}
		if err := deps.link(paths.binary, recovery); err != nil {
			_, _ = fmt.Fprintf(errOut, "Error: create uninstall recovery binary: %v\n", err)
			return 1
		}
	} else if recoveryExists {
		if err := deps.link(recovery, paths.binary); err != nil {
			_, _ = fmt.Fprintf(errOut, "Error: restore uninstall retry command: %v\n", err)
			return 1
		}
	}
	if err := deps.writeFile(paths.progress, []byte("uninstall in progress\n"), 0o600); err != nil {
		_, _ = fmt.Fprintf(errOut, "Error: record uninstall progress: %v\n", err)
		return 1
	}

	// Stop first and fail closed. Removing files while the service is still
	// running would leave an untracked process holding deleted config/binary
	// inodes until its next restart.
	service := filepath.Base(paths.unit)
	if err := deps.run("systemctl", "stop", service); err != nil &&
		!deployUninstallSystemdUnitAbsent(err, service) {
		_, _ = fmt.Fprintf(errOut, "Error: stop service %s: %v\n", service, err)
		return 1
	}
	if err := deps.run("systemctl", "disable", service); err != nil &&
		!deployUninstallSystemdUnitAbsent(err, service) {
		_, _ = fmt.Fprintf(errOut, "Error: disable service %s: %v\n", service, err)
		return 1
	}

	var failures []string
	var identityFailures []string
	manualUserRemoval := false
	manualGroupRemoval := false
	removeFile := func(path string) {
		if err := deps.remove(path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, fmt.Sprintf("remove %s: %v", path, err))
		}
	}
	removeTree := func(path string) {
		if err := deps.removeAll(path); err != nil {
			failures = append(failures, fmt.Sprintf("remove %s: %v", path, err))
		}
	}
	runCleanup := func(name string, args ...string) {
		if err := deps.run(name, args...); err != nil {
			failures = append(failures, fmt.Sprintf("run %s %s: %v", name, strings.Join(args, " "), err))
		}
	}

	removeFile(paths.binary + ".prev")
	removeTree(paths.configDir)
	removeTree(paths.logDir)

	userMarker := filepath.Join(paths.stateDir, "installer-created-user")
	groupMarker := filepath.Join(paths.stateDir, "installer-created-group")
	userID, userOwned, markerErr := deployUninstallMarkerID(deps.readFile, userMarker)
	allowGroupRemoval := userOwned
	if markerErr != nil {
		_, _ = fmt.Fprintf(out, "Preserved runtime user %s (ownership marker is unusable: %v).\n", paths.runtimeID, markerErr)
		allowGroupRemoval = false
	} else if !userOwned {
		_, _ = fmt.Fprintf(out, "Preserved runtime user %s (not recorded as installer-created).\n", paths.runtimeID)
	} else if runtimeUser, lookupErr := deps.lookupUser(paths.runtimeID); lookupErr != nil {
		if !deployUninstallUnknownUser(lookupErr) {
			identityFailures = append(identityFailures,
				fmt.Sprintf("look up runtime user %s: %v", paths.runtimeID, lookupErr),
			)
			allowGroupRemoval = false
		}
	} else if runtimeUser.Uid != userID {
		_, _ = fmt.Fprintf(out,
			"Preserved runtime user %s (current uid %s does not match installer-recorded uid %s).\n",
			paths.runtimeID, runtimeUser.Uid, userID,
		)
		allowGroupRemoval = false
	} else if err := deps.run("userdel", paths.runtimeID); err != nil {
		identityFailures = append(identityFailures,
			fmt.Sprintf("run userdel %s: %v", paths.runtimeID, err),
		)
		manualUserRemoval = true
		allowGroupRemoval = false
	}

	groupID, groupOwned, markerErr := deployUninstallMarkerID(deps.readFile, groupMarker)
	if markerErr != nil {
		_, _ = fmt.Fprintf(out, "Preserved runtime group %s (ownership marker is unusable: %v).\n", paths.runtimeID, markerErr)
	} else if !groupOwned {
		_, _ = fmt.Fprintf(out, "Preserved runtime group %s (not recorded as installer-created).\n", paths.runtimeID)
	} else if allowGroupRemoval {
		if runtimeGroup, lookupErr := deps.lookupGroup(paths.runtimeID); lookupErr != nil {
			if !deployUninstallUnknownGroup(lookupErr) {
				identityFailures = append(identityFailures,
					fmt.Sprintf("look up runtime group %s: %v", paths.runtimeID, lookupErr),
				)
			}
		} else if runtimeGroup.Gid != groupID {
			_, _ = fmt.Fprintf(out,
				"Preserved runtime group %s (current gid %s does not match installer-recorded gid %s).\n",
				paths.runtimeID, runtimeGroup.Gid, groupID,
			)
		} else if err := deps.run("groupdel", paths.runtimeID); err != nil {
			identityFailures = append(identityFailures,
				fmt.Sprintf("run groupdel %s: %v", paths.runtimeID, err),
			)
			manualGroupRemoval = true
		}
	} else {
		_, _ = fmt.Fprintf(out, "Preserved runtime group %s (runtime user ownership was not proven).\n", paths.runtimeID)
		if manualUserRemoval {
			runtimeGroup, lookupErr := deps.lookupGroup(paths.runtimeID)
			manualGroupRemoval = lookupErr == nil && runtimeGroup != nil && runtimeGroup.Gid == groupID
		}
	}

	// Keep the unit, progress marker, and executable entry points until all
	// other cleanup succeeds so a partial failure can be retried with the same
	// command.
	if len(failures) == 0 {
		removeFile(paths.unit)
	}
	if len(failures) == 0 {
		runCleanup("systemctl", "daemon-reload")
	}
	if len(failures) == 0 {
		removeTree(paths.stateDir)
	}
	if len(failures) == 0 && managedBinary {
		removeFile(paths.binary)
	}
	restoreRetryCommand := func() {
		if err := deps.link(recovery, paths.binary); err != nil && !os.IsExist(err) {
			failures = append(failures, fmt.Sprintf(
				"restore retry command %s: %v (recovery binary remains at %s)",
				paths.binary, err, recovery,
			))
		}
	}
	if len(failures) == 0 {
		failureCount := len(failures)
		removeFile(paths.progress)
		if managedBinary && len(failures) != failureCount {
			restoreRetryCommand()
		}
	}
	if len(failures) == 0 && managedBinary {
		failureCount := len(failures)
		removeFile(recovery)
		if len(failures) != failureCount {
			restoreRetryCommand()
		}
	}

	if len(failures) != 0 {
		_, _ = fmt.Fprintln(errOut, "Uninstall finished with errors:")
		for _, failure := range identityFailures {
			_, _ = fmt.Fprintf(errOut, "  - %s\n", failure)
		}
		for _, failure := range failures {
			_, _ = fmt.Fprintf(errOut, "  - %s\n", failure)
		}
		if retryExists, _ := deployUninstallPathExists(deps.lstat, paths.binary); retryExists {
			_, _ = fmt.Fprintln(errOut, "Retry with: sudo click-dog deploy uninstall")
		} else if recoveryExists, _ := deployUninstallPathExists(deps.lstat, recovery); recoveryExists {
			_, _ = fmt.Fprintf(errOut, "Retry with: sudo %s deploy uninstall\n", recovery)
		} else {
			_, _ = fmt.Fprintln(errOut, "Retry by rerunning this command from the binary used for this attempt.")
		}
		return 1
	}
	if len(identityFailures) != 0 {
		_, _ = fmt.Fprintln(errOut, "Click-Dog files were removed, but runtime identity cleanup failed:")
		for _, failure := range identityFailures {
			_, _ = fmt.Fprintf(errOut, "  - %s\n", failure)
		}
		_, _ = fmt.Fprintln(errOut, "The preserved runtime identity must be removed manually after resolving the reported cause.")
		if manualUserRemoval || manualGroupRemoval {
			_, _ = fmt.Fprintln(errOut, "After confirming no other workload uses the identity, run:")
			if manualUserRemoval {
				_, _ = fmt.Fprintf(errOut, "  sudo userdel %s\n", paths.runtimeID)
			}
			if manualGroupRemoval {
				_, _ = fmt.Fprintf(errOut, "  sudo groupdel %s\n", paths.runtimeID)
			}
		}
		_, _ = fmt.Fprintln(errOut, "This uninstaller will not retry identity removal after its ownership metadata is removed.")
		return 1
	}

	_, _ = fmt.Fprintln(out, "Click-Dog uninstalled.")
	return 0
}
