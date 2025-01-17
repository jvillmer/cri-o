package subscriptions

import (
	"bufio"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/containers/common/pkg/umask"
	"github.com/containers/storage/pkg/idtools"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	// DefaultMountsFile holds the default mount paths in the form
	// "host_path:container_path"
	DefaultMountsFile = "/usr/share/containers/mounts.conf"
	// OverrideMountsFile holds the default mount paths in the form
	// "host_path:container_path" overridden by the user
	OverrideMountsFile = "/etc/containers/mounts.conf"
	// UserOverrideMountsFile holds the default mount paths in the form
	// "host_path:container_path" overridden by the rootless user
	UserOverrideMountsFile = filepath.Join(os.Getenv("HOME"), ".config/containers/mounts.conf")
)

// subscriptionData stores the name of the file and the content read from it
type subscriptionData struct {
	name    string
	data    []byte
	mode    os.FileMode
	dirMode os.FileMode
}

// saveTo saves subscription data to given directory
func (s subscriptionData) saveTo(dir string) error {
	path := filepath.Join(dir, s.name)
	if err := os.MkdirAll(filepath.Dir(path), s.dirMode); err != nil {
		return err
	}
	return ioutil.WriteFile(path, s.data, s.mode)
}

func readAll(root, prefix string, parentMode os.FileMode) ([]subscriptionData, error) {
	path := filepath.Join(root, prefix)

	data := []subscriptionData{}

	files, err := ioutil.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return data, nil
		}

		return nil, err
	}

	for _, f := range files {
		fileData, err := readFileOrDir(root, filepath.Join(prefix, f.Name()), parentMode)
		if err != nil {
			// If the file did not exist, might be a dangling symlink
			// Ignore the error
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		data = append(data, fileData...)
	}

	return data, nil
}

func readFileOrDir(root, name string, parentMode os.FileMode) ([]subscriptionData, error) {
	path := filepath.Join(root, name)

	s, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	if s.IsDir() {
		dirData, err := readAll(root, name, s.Mode())
		if err != nil {
			return nil, err
		}
		return dirData, nil
	}
	bytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return []subscriptionData{{
		name:    name,
		data:    bytes,
		mode:    s.Mode(),
		dirMode: parentMode,
	}}, nil
}

func getHostSubscriptionData(hostDir string, mode os.FileMode) ([]subscriptionData, error) {
	var allSubscriptions []subscriptionData
	hostSubscriptions, err := readAll(hostDir, "", mode)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read subscriptions from %q", hostDir)
	}
	return append(allSubscriptions, hostSubscriptions...), nil
}

func getMounts(filePath string) []string {
	file, err := os.Open(filePath)
	if err != nil {
		// This is expected on most systems
		logrus.Debugf("File %q not found, skipping...", filePath)
		return nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if err = scanner.Err(); err != nil {
		logrus.Errorf("Reading file %q, %v skipping...", filePath, err)
		return nil
	}
	var mounts []string
	for scanner.Scan() {
		if strings.HasPrefix(strings.TrimSpace(scanner.Text()), "/") {
			mounts = append(mounts, scanner.Text())
		} else {
			logrus.Debugf("Skipping unrecognized mount in %v: %q",
				filePath, scanner.Text())
		}
	}
	return mounts
}

// getHostAndCtrDir separates the host:container paths
func getMountsMap(path string) (string, string, error) { //nolint
	arr := strings.SplitN(path, ":", 2)
	switch len(arr) {
	case 1:
		return arr[0], arr[0], nil
	case 2:
		return arr[0], arr[1], nil
	}
	return "", "", errors.Errorf("unable to get host and container dir from path: %s", path)
}

// MountsWithUIDGID copies, adds, and mounts the subscriptions to the container root filesystem
// mountLabel: MAC/SELinux label for container content
// containerWorkingDir: Private data for storing subscriptions on the host mounted in container.
// mountFile: Additional mount points required for the container.
// mountPoint: Container image mountpoint
// uid: to assign to content created for subscriptions
// gid: to assign to content created for subscriptions
// rootless: indicates whether container is running in rootless mode
// disableFips: indicates whether system should ignore fips mode
func MountsWithUIDGID(mountLabel, containerWorkingDir, mountFile, mountPoint string, uid, gid int, rootless, disableFips bool) []rspec.Mount {
	var (
		subscriptionMounts []rspec.Mount
		mountFiles         []string
	)
	// Add subscriptions from paths given in the mounts.conf files
	// mountFile will have a value if the hidden --default-mounts-file flag is set
	// Note for testing purposes only
	if mountFile == "" {
		mountFiles = append(mountFiles, []string{OverrideMountsFile, DefaultMountsFile}...)
		if rootless {
			mountFiles = append([]string{UserOverrideMountsFile}, mountFiles...)
		}
	} else {
		mountFiles = append(mountFiles, mountFile)
	}
	for _, file := range mountFiles {
		if _, err := os.Stat(file); err == nil {
			mounts, err := addSubscriptionsFromMountsFile(file, mountLabel, containerWorkingDir, uid, gid)
			if err != nil {
				logrus.Warnf("Failed to mount subscriptions, skipping entry in %s: %v", file, err)
			}
			subscriptionMounts = mounts
			break
		}
	}

	// Only add FIPS subscription mount if disableFips=false
	if disableFips {
		return subscriptionMounts
	}
	// Add FIPS mode subscription if /etc/system-fips exists on the host
	_, err := os.Stat("/etc/system-fips")
	switch {
	case err == nil:
		if err := addFIPSModeSubscription(&subscriptionMounts, containerWorkingDir, mountPoint, mountLabel, uid, gid); err != nil {
			logrus.Errorf("Adding FIPS mode subscription to container: %v", err)
		}
	case os.IsNotExist(err):
		logrus.Debug("/etc/system-fips does not exist on host, not mounting FIPS mode subscription")
	default:
		logrus.Errorf("stat /etc/system-fips failed for FIPS mode subscription: %v", err)
	}
	return subscriptionMounts
}

func rchown(chowndir string, uid, gid int) error {
	return filepath.Walk(chowndir, func(filePath string, f os.FileInfo, err error) error {
		return os.Lchown(filePath, uid, gid)
	})
}

// addSubscriptionsFromMountsFile copies the contents of host directory to container directory
// and returns a list of mounts
func addSubscriptionsFromMountsFile(filePath, mountLabel, containerWorkingDir string, uid, gid int) ([]rspec.Mount, error) {
	var mounts []rspec.Mount
	defaultMountsPaths := getMounts(filePath)
	for _, path := range defaultMountsPaths {
		hostDirOrFile, ctrDirOrFile, err := getMountsMap(path)
		if err != nil {
			return nil, err
		}
		// skip if the hostDirOrFile path doesn't exist
		fileInfo, err := os.Stat(hostDirOrFile)
		if err != nil {
			if os.IsNotExist(err) {
				logrus.Warnf("Path %q from %q doesn't exist, skipping", hostDirOrFile, filePath)
				continue
			}
			return nil, err
		}

		ctrDirOrFileOnHost := filepath.Join(containerWorkingDir, ctrDirOrFile)

		// In the event of a restart, don't want to copy subscriptions over again as they already would exist in ctrDirOrFileOnHost
		_, err = os.Stat(ctrDirOrFileOnHost)
		if os.IsNotExist(err) {

			hostDirOrFile, err = resolveSymbolicLink(hostDirOrFile)
			if err != nil {
				return nil, err
			}

			// Don't let the umask have any influence on the file and directory creation
			oldUmask := umask.Set(0)
			defer umask.Set(oldUmask)

			switch mode := fileInfo.Mode(); {
			case mode.IsDir():
				if err = os.MkdirAll(ctrDirOrFileOnHost, mode.Perm()); err != nil {
					return nil, errors.Wrap(err, "making container directory")
				}
				data, err := getHostSubscriptionData(hostDirOrFile, mode.Perm())
				if err != nil {
					return nil, errors.Wrap(err, "getting host subscription data")
				}
				for _, s := range data {
					if err := s.saveTo(ctrDirOrFileOnHost); err != nil {
						return nil, errors.Wrapf(err, "error saving data to container filesystem on host %q", ctrDirOrFileOnHost)
					}
				}
			case mode.IsRegular():
				data, err := readFileOrDir("", hostDirOrFile, mode.Perm())
				if err != nil {
					return nil, err

				}
				for _, s := range data {
					if err := os.MkdirAll(filepath.Dir(ctrDirOrFileOnHost), s.dirMode); err != nil {
						return nil, err
					}
					if err := ioutil.WriteFile(ctrDirOrFileOnHost, s.data, s.mode); err != nil {
						return nil, errors.Wrap(err, "saving data to container filesystem")
					}
				}
			default:
				return nil, errors.Errorf("unsupported file type for: %q", hostDirOrFile)
			}

			err = label.Relabel(ctrDirOrFileOnHost, mountLabel, false)
			if err != nil {
				return nil, errors.Wrap(err, "error applying correct labels")
			}
			if uid != 0 || gid != 0 {
				if err := rchown(ctrDirOrFileOnHost, uid, gid); err != nil {
					return nil, err
				}
			}
		} else if err != nil {
			return nil, err
		}

		m := rspec.Mount{
			Source:      ctrDirOrFileOnHost,
			Destination: ctrDirOrFile,
			Type:        "bind",
			Options:     []string{"bind", "rprivate"},
		}

		mounts = append(mounts, m)
	}
	return mounts, nil
}

// addFIPSModeSubscription creates /run/secrets/system-fips in the container
// root filesystem if /etc/system-fips exists on hosts.
// This enables the container to be FIPS compliant and run openssl in
// FIPS mode as the host is also in FIPS mode.
func addFIPSModeSubscription(mounts *[]rspec.Mount, containerWorkingDir, mountPoint, mountLabel string, uid, gid int) error {
	subscriptionsDir := "/run/secrets"
	ctrDirOnHost := filepath.Join(containerWorkingDir, subscriptionsDir)
	if _, err := os.Stat(ctrDirOnHost); os.IsNotExist(err) {
		if err = idtools.MkdirAllAs(ctrDirOnHost, 0755, uid, gid); err != nil { //nolint
			return err
		}
		if err = label.Relabel(ctrDirOnHost, mountLabel, false); err != nil {
			return errors.Wrapf(err, "applying correct labels on %q", ctrDirOnHost)
		}
	}
	fipsFile := filepath.Join(ctrDirOnHost, "system-fips")
	// In the event of restart, it is possible for the FIPS mode file to already exist
	if _, err := os.Stat(fipsFile); os.IsNotExist(err) {
		file, err := os.Create(fipsFile)
		if err != nil {
			return errors.Wrap(err, "creating system-fips file in container for FIPS mode")
		}
		defer file.Close()
	}

	if !mountExists(*mounts, subscriptionsDir) {
		m := rspec.Mount{
			Source:      ctrDirOnHost,
			Destination: subscriptionsDir,
			Type:        "bind",
			Options:     []string{"bind", "rprivate"},
		}
		*mounts = append(*mounts, m)
	}

	srcBackendDir := "/usr/share/crypto-policies/back-ends/FIPS"
	destDir := "/etc/crypto-policies/back-ends"
	srcOnHost := filepath.Join(mountPoint, srcBackendDir)
	if _, err := os.Stat(srcOnHost); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrap(err, "FIPS Backend directory")
	}

	if !mountExists(*mounts, destDir) {
		m := rspec.Mount{
			Source:      srcOnHost,
			Destination: destDir,
			Type:        "bind",
			Options:     []string{"bind", "rprivate"},
		}
		*mounts = append(*mounts, m)
	}
	return nil
}

// mountExists checks if a mount already exists in the spec
func mountExists(mounts []rspec.Mount, dest string) bool {
	for _, mount := range mounts {
		if mount.Destination == dest {
			return true
		}
	}
	return false
}

// resolveSymbolicLink resolves a possbile symlink path. If the path is a symlink, returns resolved
// path; if not, returns the original path.
func resolveSymbolicLink(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != os.ModeSymlink {
		return path, nil
	}
	return filepath.EvalSymlinks(path)
}
