// SPDX-License-Identifier: MIT

package selfupdate

import (
	"os"
	"testing"
)

func TestContainerDetection(t *testing.T) {
	for _, tt := range []struct {
		name, env, file, cgroup string
		want                    bool
	}{
		{name: "host"},
		{name: "systemd host", cgroup: "0::/user.slice/user-1000.slice/session-1.scope\n"},
		{name: "cgroup namespace alone", cgroup: "0::/\n"},
		{name: "docker", file: "/.dockerenv", want: true},
		{name: "podman", file: "/run/.containerenv", want: true},
		{name: "nspawn", file: "/run/systemd/container", want: true},
		{name: "container env", env: "container", want: true},
		{name: "kubernetes env", env: "KUBERNETES_SERVICE_HOST", want: true},
		{name: "windows", env: "CONTAINER_SANDBOX_MOUNT_POINT", want: true},
		{name: "docker cgroup", cgroup: "1:name=systemd:/docker/123\n", want: true},
		{name: "podman cgroup", cgroup: "0::/machine.slice/libpod-123.scope\n", want: true},
		{name: "kubernetes cgroup", cgroup: "0::/kubepods/besteffort/pod123\n", want: true},
		{name: "lxc", cgroup: "0::/lxc/test\n", want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(s string) string {
				if s == tt.env {
					return "yes"
				}
				return ""
			}
			stat := func(s string) (os.FileInfo, error) {
				if s == tt.file {
					return nil, nil
				}
				return nil, os.ErrNotExist
			}
			read := func(string) ([]byte, error) { return []byte(tt.cgroup), nil }
			if got := detectContainer(getenv, stat, read); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
