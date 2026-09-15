package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type uninstallFixture struct {
	paths           deployUninstallPaths
	commands        []string
	fail            map[string]error
	removeFail      map[string]error
	userExists      bool
	groupExists     bool
	autoRemoveGroup bool
}

func newUninstallFixture(t *testing.T) *uninstallFixture {
	t.Helper()
	root := t.TempDir()
	paths := deployUninstallPaths{
		unit:      filepath.Join(root, "etc", "systemd", "system", "click-dog.service"),
		binary:    filepath.Join(root, "usr", "local", "bin", "click-dog"),
		configDir: filepath.Join(root, "etc", "click-dog"),
		logDir:    filepath.Join(root, "var", "log", "click-dog"),
		stateDir:  filepath.Join(root, "var", "lib", "click-dog-installer"),
		progress:  filepath.Join(root, "var", "lib", "click-dog-uninstall-in-progress"),
		runtimeID: "click-dog",
	}
	for path, contents := range map[string]string{
		paths.unit:             "fixture",
		paths.binary:           "fixture",
		paths.binary + ".prev": "fixture",
		filepath.Join(paths.configDir, ".secret"):                "fixture",
		filepath.Join(paths.logDir, "click-dog.log"):             "fixture",
		filepath.Join(paths.stateDir, "installer-created-user"):  "1001\n",
		filepath.Join(paths.stateDir, "installer-created-group"): "1001\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &uninstallFixture{
		paths:       paths,
		fail:        make(map[string]error),
		removeFail:  make(map[string]error),
		userExists:  true,
		groupExists: true,
	}
}

func removeUninstallFixtureState(t *testing.T, fixture *uninstallFixture) {
	t.Helper()
	for _, path := range []string{
		fixture.paths.unit,
		fixture.paths.binary,
		fixture.paths.binary + ".prev",
		fixture.paths.binary + ".uninstalling",
		fixture.paths.configDir,
		fixture.paths.logDir,
		fixture.paths.stateDir,
		fixture.paths.progress,
	} {
		if err := os.RemoveAll(path); err != nil {
			t.Fatalf("remove fixture state %s: %v", path, err)
		}
	}
}

func (f *uninstallFixture) deps(in string, euid int) deployUninstallDeps {
	return deployUninstallDeps{
		euid:      func() int { return euid },
		in:        strings.NewReader(in),
		lstat:     os.Lstat,
		readFile:  os.ReadFile,
		writeFile: os.WriteFile,
		link:      os.Link,
		remove: func(path string) error {
			if err := f.removeFail[path]; err != nil {
				return err
			}
			return os.Remove(path)
		},
		removeAll: os.RemoveAll,
		lookupUser: func(name string) (*user.User, error) {
			if !f.userExists {
				return nil, user.UnknownUserError(name)
			}
			return &user.User{Username: name, Uid: "1001"}, nil
		},
		lookupGroup: func(name string) (*user.Group, error) {
			if !f.groupExists {
				return nil, user.UnknownGroupError(name)
			}
			return &user.Group{Name: name, Gid: "1001"}, nil
		},
		run: func(name string, args ...string) error {
			command := strings.Join(append([]string{name}, args...), " ")
			f.commands = append(f.commands, command)
			if err := f.fail[command]; err != nil {
				return err
			}
			switch command {
			case "userdel click-dog":
				f.userExists = false
				if f.autoRemoveGroup {
					f.groupExists = false
				}
			case "groupdel click-dog":
				f.groupExists = false
			}
			return nil
		},
	}
}

func TestRunDeployUninstall_Help(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runDeployUninstall([]string{"--help"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errOut.String(), "sudo click-dog deploy uninstall [--yes]") {
		t.Errorf("help missing usage:\n%s", errOut.String())
	}
}

func TestDeployUninstallSystemdUnitAbsent(t *testing.T) {
	service := "click-dog.service"
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "not loaded", err: errors.New("exit status 5: Unit click-dog.service not loaded."), want: true},
		{name: "unit does not exist", err: errors.New("Unit click-dog.service does not exist."), want: true},
		{name: "systemd 255 disable message", err: errors.New("exit status 1: Failed to disable unit, unit click-dog.service does not exist."), want: true},
		{name: "unit file does not exist", err: errors.New("Unit file click-dog.service does not exist."), want: true},
		{name: "masked is not proof of stopped", err: errors.New("Unit click-dog.service is masked."), want: false},
		{name: "different unit", err: errors.New("Unit other.service not loaded."), want: false},
		{name: "dependency missing", err: errors.New("Dependency unit click-dog.service not found."), want: false},
		{name: "ordinary failure", err: errors.New("access denied"), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := deployUninstallSystemdUnitAbsent(test.err, service); got != test.want {
				t.Fatalf("deployUninstallSystemdUnitAbsent(%q) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestRunDeployUninstall_RequiresRoot(t *testing.T) {
	fixture := newUninstallFixture(t)
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 1000))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "requires root") {
		t.Errorf("stderr missing root requirement:\n%s", errOut.String())
	}
	if len(fixture.commands) != 0 {
		t.Errorf("commands ran before root check: %v", fixture.commands)
	}
}

func TestRunDeployUninstall_DefaultConfirmationAborts(t *testing.T) {
	fixture := newUninstallFixture(t)
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith(nil, &out, &errOut, fixture.paths, fixture.deps("n\n", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "including credentials") ||
		!strings.Contains(out.String(), "ClickHouse: unchanged") ||
		!strings.Contains(out.String(), "Aborted.") {
		t.Errorf("stdout missing destructive scope or abort:\n%s", out.String())
	}
	if len(fixture.commands) != 0 {
		t.Errorf("commands ran after declined confirmation: %v", fixture.commands)
	}
	if _, err := os.Stat(fixture.paths.binary); err != nil {
		t.Errorf("binary changed after declined confirmation: %v", err)
	}
}

func TestRunDeployUninstall_EmptyConfirmationAborts(t *testing.T) {
	fixture := newUninstallFixture(t)
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith(nil, &out, &errOut, fixture.paths, fixture.deps("\n", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "Aborted.") {
		t.Errorf("stdout missing fail-safe abort:\n%s", out.String())
	}
	if len(fixture.commands) != 0 {
		t.Errorf("commands ran after empty confirmation: %v", fixture.commands)
	}
}

func TestRunDeployUninstall_YesRemovesStandardInstall(t *testing.T) {
	fixture := newUninstallFixture(t)
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}

	wantCommands := []string{
		"systemctl stop click-dog.service",
		"systemctl disable click-dog.service",
		"userdel click-dog",
		"groupdel click-dog",
		"systemctl daemon-reload",
	}
	if !reflect.DeepEqual(fixture.commands, wantCommands) {
		t.Errorf("commands = %v, want %v", fixture.commands, wantCommands)
	}
	for _, path := range []string{
		fixture.paths.unit,
		fixture.paths.binary,
		fixture.paths.binary + ".prev",
		fixture.paths.binary + ".uninstalling",
		fixture.paths.configDir,
		fixture.paths.logDir,
		fixture.paths.stateDir,
		fixture.paths.progress,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists or stat failed unexpectedly: %v", path, err)
		}
	}
	if !strings.Contains(out.String(), "Click-Dog uninstalled.") {
		t.Errorf("stdout missing completion:\n%s", out.String())
	}
}

func TestRunDeployUninstall_StopFailureLeavesFiles(t *testing.T) {
	fixture := newUninstallFixture(t)
	fixture.fail["systemctl stop click-dog.service"] = errors.New("stop failed")
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "stop service click-dog.service") {
		t.Errorf("stderr missing stop failure:\n%s", errOut.String())
	}
	if _, err := os.Stat(fixture.paths.binary); err != nil {
		t.Errorf("binary removed after stop failure: %v", err)
	}
	if got := fixture.commands; !reflect.DeepEqual(got, []string{"systemctl stop click-dog.service"}) {
		t.Errorf("commands = %v, want only stop", got)
	}
}

func TestRunDeployUninstall_DisableFailureLeavesFiles(t *testing.T) {
	fixture := newUninstallFixture(t)
	fixture.fail["systemctl disable click-dog.service"] = errors.New("disable failed")
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "disable service click-dog.service") {
		t.Errorf("stderr missing disable failure:\n%s", errOut.String())
	}
	if _, err := os.Stat(fixture.paths.binary); err != nil {
		t.Errorf("binary removed after disable failure: %v", err)
	}
	wantCommands := []string{"systemctl stop click-dog.service", "systemctl disable click-dog.service"}
	if !reflect.DeepEqual(fixture.commands, wantCommands) {
		t.Errorf("commands = %v, want %v", fixture.commands, wantCommands)
	}
}

func TestRunDeployUninstall_ManagedCleanupFailureIsRetriable(t *testing.T) {
	fixture := newUninstallFixture(t)
	fixture.removeFail[fixture.paths.unit] = errors.New("unit busy")
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "Uninstall finished with errors") ||
		!strings.Contains(errOut.String(), "remove "+fixture.paths.unit) {
		t.Errorf("stderr missing cleanup failure:\n%s", errOut.String())
	}
	if _, err := os.Stat(fixture.paths.unit); err != nil {
		t.Errorf("systemd unit removed before cleanup completed: %v", err)
	}
	if _, err := os.Stat(fixture.paths.progress); err != nil {
		t.Errorf("retry marker missing after cleanup failure: %v", err)
	}
	if _, err := os.Stat(fixture.paths.binary); err != nil {
		t.Errorf("retry command missing after cleanup failure: %v", err)
	}

	delete(fixture.removeFail, fixture.paths.unit)
	fixture.commands = nil
	out.Reset()
	errOut.Reset()
	code = runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("retry exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(fixture.paths.progress); !os.IsNotExist(err) {
		t.Errorf("retry marker remains after successful retry: %v", err)
	}
}

func TestRunDeployUninstall_IdentityFailurePreservesIdentityAndCompletesManagedCleanup(t *testing.T) {
	for _, test := range []struct {
		name         string
		command      string
		failure      error
		userRemains  bool
		groupRemains bool
		wantCommands []string
		wantGuidance []string
	}{
		{
			name:         "busy runtime user",
			command:      "userdel click-dog",
			failure:      errors.New("user click-dog is currently used by process 3248"),
			userRemains:  true,
			groupRemains: true,
			wantCommands: []string{
				"systemctl stop click-dog.service",
				"systemctl disable click-dog.service",
				"userdel click-dog",
				"systemctl daemon-reload",
			},
			wantGuidance: []string{"sudo userdel click-dog", "sudo groupdel click-dog"},
		},
		{
			name:         "group is another user's primary group",
			command:      "groupdel click-dog",
			failure:      errors.New("cannot remove the primary group of user 'cd-other'"),
			groupRemains: true,
			wantCommands: []string{
				"systemctl stop click-dog.service",
				"systemctl disable click-dog.service",
				"userdel click-dog",
				"groupdel click-dog",
				"systemctl daemon-reload",
			},
			wantGuidance: []string{"sudo groupdel click-dog"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newUninstallFixture(t)
			fixture.fail[test.command] = test.failure

			var out, errOut bytes.Buffer
			code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
			if code != 1 {
				t.Fatalf("exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
			}
			if !strings.Contains(errOut.String(), "Click-Dog files were removed, but runtime identity cleanup failed") ||
				!strings.Contains(errOut.String(), test.command) ||
				!strings.Contains(errOut.String(), "must be removed manually") ||
				!strings.Contains(errOut.String(), "will not retry identity removal") {
				t.Errorf("stderr missing identity-preservation guidance:\n%s", errOut.String())
			}
			if strings.Contains(errOut.String(), "Retry with:") {
				t.Errorf("identity-only failure incorrectly recommends retrying uninstall:\n%s", errOut.String())
			}
			for _, guidance := range test.wantGuidance {
				if !strings.Contains(errOut.String(), guidance) {
					t.Errorf("stderr missing manual command %q:\n%s", guidance, errOut.String())
				}
			}
			if fixture.userExists != test.userRemains || fixture.groupExists != test.groupRemains {
				t.Errorf("identity state = user:%v group:%v, want user:%v group:%v",
					fixture.userExists, fixture.groupExists, test.userRemains, test.groupRemains)
			}
			if !reflect.DeepEqual(fixture.commands, test.wantCommands) {
				t.Errorf("commands = %v, want managed teardown to continue as %v", fixture.commands, test.wantCommands)
			}
			for _, path := range []string{
				fixture.paths.unit,
				fixture.paths.binary,
				fixture.paths.binary + ".prev",
				fixture.paths.binary + ".uninstalling",
				fixture.paths.configDir,
				fixture.paths.logDir,
				fixture.paths.stateDir,
				fixture.paths.progress,
			} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Errorf("managed path %s remains after identity failure: %v", path, err)
				}
			}

			fixture.commands = nil
			out.Reset()
			errOut.Reset()
			code = runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
			if code != 0 {
				t.Fatalf("repeat exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
			}
			if !strings.Contains(out.String(), "already absent") {
				t.Errorf("repeat stdout missing converged no-op:\n%s", out.String())
			}
			if len(fixture.commands) != 0 {
				t.Errorf("repeat commands = %v, want none", fixture.commands)
			}
		})
	}
}

func TestRunDeployUninstall_LateFailureRestoresRetryCommand(t *testing.T) {
	fixture := newUninstallFixture(t)
	fixture.removeFail[fixture.paths.progress] = errors.New("progress busy")

	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "remove "+fixture.paths.progress) {
		t.Errorf("stderr missing late cleanup failure:\n%s", errOut.String())
	}
	for _, path := range []string{fixture.paths.binary, fixture.paths.binary + ".uninstalling"} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("retry entry point %s missing after late failure: %v", path, err)
		}
	}

	delete(fixture.removeFail, fixture.paths.progress)
	fixture.commands = nil
	out.Reset()
	errOut.Reset()
	code = runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("retry exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	for _, path := range []string{fixture.paths.binary, fixture.paths.binary + ".uninstalling", fixture.paths.progress} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("finalized path %s remains after retry: %v", path, err)
		}
	}
}

func TestRunDeployUninstall_RetryCommandRunsInNewProcess(t *testing.T) {
	if os.Getenv("CLICK_DOG_UNINSTALL_HELPER") == "1" {
		paths := deployUninstallPaths{
			unit:      os.Getenv("CLICK_DOG_UNINSTALL_UNIT"),
			binary:    os.Getenv("CLICK_DOG_UNINSTALL_BINARY"),
			configDir: os.Getenv("CLICK_DOG_UNINSTALL_CONFIG"),
			logDir:    os.Getenv("CLICK_DOG_UNINSTALL_LOG"),
			stateDir:  os.Getenv("CLICK_DOG_UNINSTALL_STATE"),
			progress:  os.Getenv("CLICK_DOG_UNINSTALL_PROGRESS"),
			runtimeID: "click-dog",
		}
		fixture := &uninstallFixture{
			paths:       paths,
			fail:        make(map[string]error),
			removeFail:  make(map[string]error),
			userExists:  false,
			groupExists: false,
		}
		if code := runDeployUninstallWith([]string{"--yes"}, os.Stdout, os.Stderr, paths, fixture.deps("", 0)); code != 0 {
			os.Exit(code)
		}
		return
	}

	fixture := newUninstallFixture(t)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fixture.paths.binary); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(testBinary, fixture.paths.binary); err != nil {
		t.Fatalf("link test executable as installed command: %v", err)
	}
	fixture.removeFail[fixture.paths.binary+".uninstalling"] = errors.New("recovery busy")

	var out, errOut bytes.Buffer
	if code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0)); code != 1 {
		t.Fatalf("first exit = %d, want 1", code)
	}
	if _, err := os.Stat(fixture.paths.progress); !os.IsNotExist(err) {
		t.Fatalf("progress marker should be gone before recovery-link failure: %v", err)
	}

	cmd := exec.Command(fixture.paths.binary, "-test.run=^TestRunDeployUninstall_RetryCommandRunsInNewProcess$")
	cmd.Env = append(os.Environ(),
		"CLICK_DOG_UNINSTALL_HELPER=1",
		"CLICK_DOG_UNINSTALL_UNIT="+fixture.paths.unit,
		"CLICK_DOG_UNINSTALL_BINARY="+fixture.paths.binary,
		"CLICK_DOG_UNINSTALL_CONFIG="+fixture.paths.configDir,
		"CLICK_DOG_UNINSTALL_LOG="+fixture.paths.logDir,
		"CLICK_DOG_UNINSTALL_STATE="+fixture.paths.stateDir,
		"CLICK_DOG_UNINSTALL_PROGRESS="+fixture.paths.progress,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("retry through restored command failed: %v\n%s", err, output)
	}
	for _, path := range []string{fixture.paths.binary, fixture.paths.binary + ".uninstalling", fixture.paths.progress} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("finalized path %s remains after process-level retry: %v", path, err)
		}
	}
}

func TestRunDeployUninstall_UserdelAutoRemovesPrivateGroup(t *testing.T) {
	fixture := newUninstallFixture(t)
	fixture.autoRemoveGroup = true
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	for _, command := range fixture.commands {
		if command == "groupdel click-dog" {
			t.Errorf("groupdel ran after userdel already removed the private group: %v", fixture.commands)
		}
	}
}

func TestRunDeployUninstall_PreservesPreexistingRuntimeIdentity(t *testing.T) {
	fixture := newUninstallFixture(t)
	for _, marker := range []string{
		filepath.Join(fixture.paths.stateDir, "installer-created-user"),
		filepath.Join(fixture.paths.stateDir, "installer-created-group"),
	} {
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !fixture.userExists || !fixture.groupExists {
		t.Fatal("preexisting runtime identity was deleted")
	}
	if !strings.Contains(out.String(), "Preserved runtime user") ||
		!strings.Contains(out.String(), "Preserved runtime group") {
		t.Errorf("stdout missing preservation notice:\n%s", out.String())
	}
	for _, command := range fixture.commands {
		if strings.HasPrefix(command, "userdel ") || strings.HasPrefix(command, "groupdel ") {
			t.Errorf("account-management command ran for preexisting identity: %v", fixture.commands)
		}
	}
}

func TestRunDeployUninstall_PreservesChangedRuntimeIdentity(t *testing.T) {
	fixture := newUninstallFixture(t)
	userMarker := filepath.Join(fixture.paths.stateDir, "installer-created-user")
	if err := os.WriteFile(userMarker, []byte("2002\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !fixture.userExists || !fixture.groupExists {
		t.Fatal("changed runtime identity was deleted")
	}
	if !strings.Contains(out.String(), "current uid 1001 does not match installer-recorded uid 2002") {
		t.Errorf("stdout missing numeric identity mismatch:\n%s", out.String())
	}
	for _, command := range fixture.commands {
		if strings.HasPrefix(command, "userdel ") || strings.HasPrefix(command, "groupdel ") {
			t.Errorf("account-management command ran after identity mismatch: %v", fixture.commands)
		}
	}
	if _, err := os.Stat(fixture.paths.unit); !os.IsNotExist(err) {
		t.Errorf("systemd unit remains after safe identity preservation: %v", err)
	}
	if _, err := os.Stat(fixture.paths.progress); !os.IsNotExist(err) {
		t.Errorf("retry marker remains after successful teardown: %v", err)
	}
}

func TestRunDeployUninstall_PreservesIdentityWithInvalidMarker(t *testing.T) {
	fixture := newUninstallFixture(t)
	userMarker := filepath.Join(fixture.paths.stateDir, "installer-created-user")
	if err := os.WriteFile(userMarker, []byte("not-a-uid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !fixture.userExists || !fixture.groupExists {
		t.Fatal("runtime identity with unusable ownership proof was deleted")
	}
	if !strings.Contains(out.String(), "ownership marker is unusable") ||
		!strings.Contains(out.String(), "runtime user ownership was not proven") {
		t.Errorf("stdout missing preservation reason:\n%s", out.String())
	}
	for _, command := range fixture.commands {
		if strings.HasPrefix(command, "userdel ") || strings.HasPrefix(command, "groupdel ") {
			t.Errorf("account-management command ran without valid ownership proof: %v", fixture.commands)
		}
	}
	if _, err := os.Stat(fixture.paths.unit); !os.IsNotExist(err) {
		t.Errorf("systemd unit remains after safe identity preservation: %v", err)
	}
}

func TestRunDeployUninstall_CompletesWhenInstalledBinaryIsMissing(t *testing.T) {
	fixture := newUninstallFixture(t)
	if err := os.Remove(fixture.paths.binary); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	for _, path := range []string{
		fixture.paths.unit,
		fixture.paths.binary + ".prev",
		fixture.paths.configDir,
		fixture.paths.logDir,
		fixture.paths.stateDir,
		fixture.paths.progress,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists or stat failed unexpectedly: %v", path, err)
		}
	}
}

func TestRunDeployUninstall_ResumesAfterUnitWasRemoved(t *testing.T) {
	fixture := newUninstallFixture(t)
	fixture.fail["systemctl daemon-reload"] = errors.New("reload failed")
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 1 {
		t.Fatalf("first exit = %d, want 1", code)
	}
	if _, err := os.Stat(fixture.paths.unit); !os.IsNotExist(err) {
		t.Errorf("unit should be removed before daemon-reload: %v", err)
	}
	if _, err := os.Stat(fixture.paths.progress); err != nil {
		t.Errorf("retry marker missing after daemon-reload failure: %v", err)
	}

	delete(fixture.fail, "systemctl daemon-reload")
	fixture.fail["systemctl stop click-dog.service"] = errors.New("Unit click-dog.service not loaded")
	fixture.fail["systemctl disable click-dog.service"] = errors.New("Unit file click-dog.service does not exist")
	fixture.commands = nil
	out.Reset()
	errOut.Reset()
	code = runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("retry exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	wantCommands := []string{
		"systemctl stop click-dog.service",
		"systemctl disable click-dog.service",
		"systemctl daemon-reload",
	}
	if got := fixture.commands; !reflect.DeepEqual(got, wantCommands) {
		t.Errorf("retry commands = %v, want %v", got, wantCommands)
	}
}

func TestRunDeployUninstall_RejectsNonSystemdDeployment(t *testing.T) {
	fixture := newUninstallFixture(t)
	removeUninstallFixtureState(t, fixture)
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith(nil, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "no standard systemd installation") ||
		!strings.Contains(errOut.String(), "orchestrator") {
		t.Errorf("stderr missing deployment guidance:\n%s", errOut.String())
	}
}

func TestRunDeployUninstall_AlreadyAbsentIsSuccessWithYes(t *testing.T) {
	fixture := newUninstallFixture(t)
	removeUninstallFixtureState(t, fixture)
	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "already absent") {
		t.Errorf("stdout missing idempotent no-op message:\n%s", out.String())
	}
	if len(fixture.commands) != 0 {
		t.Errorf("commands ran for already-absent installation: %v", fixture.commands)
	}
	if _, err := os.Stat(fixture.paths.binary); !os.IsNotExist(err) {
		t.Errorf("already-absent binary appeared during no-op: %v", err)
	}
}

func TestRunDeployUninstall_CleansResidualStateWhenUnitIsMissing(t *testing.T) {
	fixture := newUninstallFixture(t)
	if err := os.Remove(fixture.paths.unit); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := runDeployUninstallWith([]string{"--yes"}, &out, &errOut, fixture.paths, fixture.deps("", 0))
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "already absent") {
		t.Errorf("residual installation was incorrectly treated as absent:\n%s", out.String())
	}
	if got := fixture.commands; !reflect.DeepEqual(got, []string{
		"systemctl stop click-dog.service",
		"systemctl disable click-dog.service",
		"userdel click-dog",
		"groupdel click-dog",
		"systemctl daemon-reload",
	}) {
		t.Errorf("commands = %v, want service shutdown before residual cleanup", got)
	}
	for _, path := range []string{
		fixture.paths.binary,
		fixture.paths.binary + ".prev",
		fixture.paths.configDir,
		fixture.paths.logDir,
		fixture.paths.stateDir,
		fixture.paths.progress,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("residual path %s remains after cleanup: %v", path, err)
		}
	}
}
