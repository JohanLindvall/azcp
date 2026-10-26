// SPDX-License-Identifier: MIT

package selfupdate

import (
	"os"
	"strings"
)

// ContainerBuild is stamped into the official image. Unlike runtime hints it
// cannot disappear when a runtime hides cgroups or the environment is cleared.
var ContainerBuild = "false"

// InContainer also catches ordinary release binaries copied into containers.
func InContainer() bool {
	return ContainerBuild == "true" || detectContainer(os.Getenv, os.Stat, os.ReadFile)
}

func detectContainer(getenv func(string) string, stat func(string) (os.FileInfo, error), readFile func(string) ([]byte, error)) bool {
	for _, name := range []string{"container", "KUBERNETES_SERVICE_HOST", "CONTAINER_SANDBOX_MOUNT_POINT"} {
		if getenv(name) != "" {
			return true
		}
	}
	for _, name := range []string{"/.dockerenv", "/run/.containerenv", "/run/systemd/container"} {
		if _, err := stat(name); err == nil {
			return true
		}
	}
	for _, name := range []string{"/proc/1/cgroup", "/proc/self/cgroup"} {
		b, _ := readFile(name)
		for _, marker := range []string{"docker", "kubepods", "containerd", "libpod", "/lxc/", "/lxc.payload."} {
			if strings.Contains(string(b), marker) {
				return true
			}
		}
	}
	return false
}
