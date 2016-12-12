package allocdir

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
)

type TaskDir struct {
	// Dir is the path to Task directory on the host
	Dir string

	// SharedAllocDir is the path to shared alloc directory on the host
	// <alloc_dir>/alloc/
	SharedAllocDir string

	// SharedTaskDir is the path to the shared alloc directory linked into
	// the task directory on the host.
	// <task_dir>/alloc/
	SharedTaskDir string

	// LocalDir is the path to the task's local directory on the host
	// <task_dir>/local/
	LocalDir string

	// LogDir is the path to the task's log directory on the host
	// <alloc_dir>/alloc/logs/
	LogDir string

	// SecretsDir is the path to secrets/ directory on the host
	// <task_dir>/secrets/
	SecretsDir string
}

// NewTaskDir creates a TaskDir struct with paths set. Call Build() to
// create paths on disk.
func NewTaskDir(allocDir, taskName string) *TaskDir {
	taskDir := filepath.Join(allocDir, taskName)
	return &TaskDir{
		Dir:            taskDir,
		SharedAllocDir: filepath.Join(allocDir, SharedAllocName),
		LogDir:         filepath.Join(allocDir, SharedAllocName, LogDirName),
		SharedTaskDir:  filepath.Join(taskDir, SharedAllocName),
		LocalDir:       filepath.Join(taskDir, TaskLocal),
		SecretsDir:     filepath.Join(taskDir, TaskSecrets),
	}
}

// Build default directories and permissions in a task directory.
func (t *TaskDir) Build() error {
	if err := os.MkdirAll(t.Dir, 0777); err != nil {
		return err
	}

	// Make the task directory have non-root permissions.
	if err := dropDirPermissions(t.Dir); err != nil {
		return err
	}

	// Create a local directory that each task can use.
	if err := os.MkdirAll(t.LocalDir, 0777); err != nil {
		return err
	}

	if err := dropDirPermissions(t.LocalDir); err != nil {
		return err
	}

	// Create the directories that should be in every task.
	for _, dir := range TaskDirs {
		absdir := filepath.Join(t.Dir, dir)
		if err := os.MkdirAll(absdir, 0777); err != nil {
			return err
		}

		if err := dropDirPermissions(absdir); err != nil {
			return err
		}
	}

	// Create the secret directory
	if err := createSecretDir(t.SecretsDir); err != nil {
		return err
	}

	if err := dropDirPermissions(t.SecretsDir); err != nil {
		return err
	}

	return nil
}

// MountSharedDir mounts the shared alloc dir inside the task directory.
func (t *TaskDir) MountSharedDir() error {
	if err := linkDir(t.SharedAllocDir, t.SharedTaskDir); err != nil {
		return fmt.Errorf("Failed to mount shared directory for task: %v", err)
	}
	return nil
}

// BuildChroot takes a mapping of absolute directory or file paths on the host
// to their intended, relative location within the task directory. This
// attempts hardlink and then defaults to copying. If the path exists on the
// host and can't be embedded an error is returned.
func (t *TaskDir) BuildChroot(entries map[string]string) error {
	// Link/copy chroot entries
	if err := t.embedDirs(entries); err != nil {
		return err
	}

	// Mount special dirs
	if err := t.mountSpecialDirs(); err != nil {
		return err
	}

	return nil
}

func (t *TaskDir) embedDirs(entries map[string]string) error {
	subdirs := make(map[string]string)
	for source, dest := range entries {
		// Check to see if directory exists on host.
		s, err := os.Stat(source)
		if os.IsNotExist(err) {
			continue
		}

		// Embedding a single file
		if !s.IsDir() {
			if err := createDir(t.Dir, filepath.Dir(dest)); err != nil {
				return fmt.Errorf("Couldn't create destination directory %v: %v", dest, err)
			}

			// Copy the file.
			taskEntry := filepath.Join(t.Dir, dest)
			if err := linkOrCopy(source, taskEntry, s.Mode().Perm()); err != nil {
				return err
			}

			continue
		}

		// Create destination directory.
		destDir := filepath.Join(t.Dir, dest)

		if err := createDir(t.Dir, dest); err != nil {
			return fmt.Errorf("Couldn't create destination directory %v: %v", destDir, err)
		}

		// Enumerate the files in source.
		dirEntries, err := ioutil.ReadDir(source)
		if err != nil {
			return fmt.Errorf("Couldn't read directory %v: %v", source, err)
		}

		for _, entry := range dirEntries {
			hostEntry := filepath.Join(source, entry.Name())
			taskEntry := filepath.Join(destDir, filepath.Base(hostEntry))
			if entry.IsDir() {
				subdirs[hostEntry] = filepath.Join(dest, filepath.Base(hostEntry))
				continue
			}

			// Check if entry exists. This can happen if restarting a failed
			// task.
			if _, err := os.Lstat(taskEntry); err == nil {
				continue
			}

			if !entry.Mode().IsRegular() {
				// If it is a symlink we can create it, otherwise we skip it.
				if entry.Mode()&os.ModeSymlink == 0 {
					continue
				}

				link, err := os.Readlink(hostEntry)
				if err != nil {
					return fmt.Errorf("Couldn't resolve symlink for %v: %v", source, err)
				}

				if err := os.Symlink(link, taskEntry); err != nil {
					// Symlinking twice
					if err.(*os.LinkError).Err.Error() != "file exists" {
						return fmt.Errorf("Couldn't create symlink: %v", err)
					}
				}
				continue
			}

			if err := linkOrCopy(hostEntry, taskEntry, entry.Mode().Perm()); err != nil {
				return err
			}
		}
	}

	// Recurse on self to copy subdirectories.
	if len(subdirs) != 0 {
		return t.embedDirs(subdirs)
	}

	return nil
}