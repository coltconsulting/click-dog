package updater

import (
	"fmt"
	"os"
)

// EnsureSecureBinDir fails closed if dir is writable by group or other without
// the sticky bit set.
//
// Self-update stages, smoke-tests (executes), and renames a binary inside the
// install directory, typically as root. If that directory is writable by an
// unprivileged user, they can replace the staged file — or the canonical
// binary or its .prev backup — between extraction and execution, regardless of
// the staged file's own mode (directory-entry replacement is governed by the
// parent directory's permissions, not the entry's). The sticky bit (as on
// /tmp) closes that hole by restricting rename/unlink to the entry's owner, so
// it is treated as acceptable. Anything else group/other-writable is refused.
func EnsureSecureBinDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat install directory %s: %w", dir, err)
	}
	mode := info.Mode()
	if mode.Perm()&0o022 != 0 && mode&os.ModeSticky == 0 {
		return fmt.Errorf("install directory %s is writable by group/other (mode %#o) "+
			"without the sticky bit; an unprivileged user could replace the binary "+
			"mid-update. Lock it down (e.g. chmod g-w,o-w %s) and retry", dir, mode.Perm(), dir)
	}
	return nil
}
