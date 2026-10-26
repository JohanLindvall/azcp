// SPDX-License-Identifier: MIT

package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"
)

func TestStableVersions(t *testing.T) {
	for _, v := range []string{"v1.0.0", "0.6.6", "v123.456.789"} {
		if _, ok := releaseVersion(v); !ok {
			t.Errorf("rejected %q", v)
		}
	}
	for _, v := range []string{"dev", "(devel)", "v1.0", "v1.0.0-rc.1", "v1.0.0-12-g123", "v1.0.0-dirty", "v0.0.0-20261026-deadbeef", "v1.0.0+local", "v01.0.0", "v+1.0.0", "v1.0.0/../../azcp", "v99999999999999999999999.0.0"} {
		if _, ok := releaseVersion(v); ok {
			t.Errorf("accepted %q", v)
		}
	}
	for _, tt := range []struct {
		latest, current string
		want            bool
	}{
		{"v1.10.0", "v1.9.99", true}, {"v2.0.0", "v1.99.99", true},
		{"v1.0.2", "v1.0.1", true}, {"v1.0.0", "1.0.0", false},
		{"v1.9.0", "v1.10.0", false}, {"v1.0.1-rc.1", "v1.0.0", false},
	} {
		if got := newer(tt.latest, tt.current); got != tt.want {
			t.Errorf("newer(%s, %s) = %v", tt.latest, tt.current, got)
		}
	}
}

func TestManifestRejectsAmbiguousOrUnsafeAssets(t *testing.T) {
	line := fmt.Sprintf("%x  azcp_v1.2.3_linux_amd64.bin.gz\n", sha256.Sum256([]byte("asset")))
	r, err := parseManifest(line, "_linux_amd64.bin.gz")
	if err != nil || r.version != "v1.2.3" {
		t.Fatalf("release = %+v, %v", r, err)
	}
	for _, manifest := range []string{
		"", line + line, strings.ReplaceAll(line, "v1.2.3", "v1.2.3-rc.1"),
		strings.ReplaceAll(line, "v1.2.3", "v1.2.3/../../bad"),
		strings.ReplaceAll(line, "linux", "darwin"), "broken checksum\n", "00" + line[64:],
		strings.Repeat("z", 64) + line[64:],
	} {
		if _, err := parseManifest(manifest, "_linux_amd64.bin.gz"); err == nil {
			t.Errorf("accepted %q", manifest)
		}
	}
}

type fixture struct {
	u              updater
	cfg            Config
	manifest       string
	archive        []byte
	status         int
	requests       atomic.Int32
	beforeDownload func()
}

func newFixture(t *testing.T, goos string) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{cfg: Config{Version: "v1.0.0", VersionCheck: true, Install: true}, status: http.StatusOK}
	f.u = updater{goos: goos, goarch: "amd64", executable: filepath.Join(dir, "azcp"), cacheDir: filepath.Join(dir, "cache"),
		now:      func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) },
		validate: func(string, string, string) error { return nil }}
	if runtime.GOOS == "windows" {
		f.u.executable += ".exe"
	}
	if err := os.WriteFile(f.u.executable, []byte("original"), 0o751); err != nil {
		t.Fatal(err)
	}
	f.setRelease(t, "v1.10.0", "replacement")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		w.WriteHeader(f.status)
		if r.URL.Path == "/latest/download/SHA256SUMS" {
			io.WriteString(w, f.manifest)
		} else if strings.HasPrefix(r.URL.Path, "/download/") {
			if f.beforeDownload != nil {
				f.beforeDownload()
			}
			w.Write(f.archive)
		}
	}))
	t.Cleanup(srv.Close)
	f.u.client, f.u.baseURL = srv.Client(), srv.URL
	return f
}

func (f *fixture) setRelease(t *testing.T, version, payload string) {
	t.Helper()
	var b bytes.Buffer
	if f.u.goos == "windows" {
		z := zip.NewWriter(&b)
		w, err := z.Create("azcp.exe")
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, payload)
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		z := gzip.NewWriter(&b)
		io.WriteString(z, payload)
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
	}
	f.archive = b.Bytes()
	f.manifest = fmt.Sprintf("%x  azcp_%s%s\n", sha256.Sum256(f.archive), version, assetSuffix(f.u.goos, f.u.goarch))
}

func assertContents(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Fatalf("%s: %q, %v; want %q", path, b, err, want)
	}
}

func TestInstallAndDailyCheck(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			f := newFixture(t, goos)
			r, err := f.u.run(t.Context(), f.cfg)
			if err != nil || !r.Installed || r.Version != "v1.10.0" {
				t.Fatalf("result %+v, %v", r, err)
			}
			assertContents(t, f.u.executable, "replacement")
			// A long-running older invocation must not reinstall the update.
			if _, err := f.u.run(t.Context(), f.cfg); err == nil {
				t.Fatal("explicit update ignored a concurrently replaced executable")
			}
			f.cfg.Install = false
			if r, err := f.u.run(t.Context(), f.cfg); err != nil || r != (Result{}) {
				t.Fatalf("older running invocation: %+v, %v", r, err)
			}
			if runtime.GOOS != "windows" {
				info, err := os.Stat(f.u.executable)
				if err != nil || info.Mode().Perm() != 0o751 {
					t.Fatalf("mode not preserved: %v, %v", info, err)
				}
			}
			f.cfg.Version = r.Version
			if _, err := f.u.run(t.Context(), f.cfg); err != nil {
				t.Fatal(err)
			}
			if n := f.requests.Load(); n != 2 {
				t.Fatalf("cached run made requests: %d", n)
			}
			later := f.u.now().Add(checkInterval)
			f.u.now = func() time.Time { return later }
			if _, err := f.u.run(t.Context(), f.cfg); err != nil {
				t.Fatal(err)
			}
			if n := f.requests.Load(); n != 3 {
				t.Fatalf("daily check: %d requests", n)
			}
			for _, pattern := range []string{filepath.Join(filepath.Dir(f.u.executable), ".azcp-new-*"), filepath.Join(f.u.cacheDir, "download-*")} {
				if paths, _ := filepath.Glob(pattern); len(paths) != 0 {
					t.Fatalf("staging files remain: %v", paths)
				}
			}
		})
	}
}

func TestExplicitUpdateBypassesDailyCheck(t *testing.T) {
	f := newFixture(t, "linux")
	f.cfg.Install = false
	r, err := f.u.run(t.Context(), f.cfg)
	if err != nil || r.Installed || r.Version != "v1.10.0" {
		t.Fatalf("result %+v, %v", r, err)
	}
	assertContents(t, f.u.executable, "original")
	if f.requests.Load() != 1 {
		t.Fatal("check-only downloaded an executable")
	}
	if _, err := f.u.run(t.Context(), f.cfg); err != nil || f.requests.Load() != 1 {
		t.Fatalf("automatic check ignored daily cache: %v", err)
	}
	f.cfg.Install = true
	if r, err := f.u.run(t.Context(), f.cfg); err != nil || !r.Installed {
		t.Fatalf("opt back in: %+v, %v", r, err)
	}
}

func TestDisabledAndDevelopmentRunsDoNoIO(t *testing.T) {
	for _, cfg := range []Config{
		{Version: "v1.0.0"},
		{Version: "dev", VersionCheck: true},
		{Version: "v1.0.0-dirty", VersionCheck: true},
	} {
		// Deliberately no client, clock or paths: any IO would fail or panic.
		if r, err := (&updater{goos: "linux", goarch: "amd64"}).run(t.Context(), cfg); err != nil || r != (Result{}) {
			t.Fatalf("%+v: %+v, %v", cfg, r, err)
		}
	}
	old := ContainerBuild
	ContainerBuild = "true"
	t.Cleanup(func() { ContainerBuild = old })
	if r, err := Run(t.Context(), Config{Version: "v1.0.0", VersionCheck: true}); err != nil || r != (Result{}) {
		t.Fatalf("container: %+v, %v", r, err)
	}
	if _, err := Run(t.Context(), Config{Version: "v1.0.0", Install: true}); err == nil {
		t.Fatal("explicit container update silently succeeded")
	}
}

func TestFailedUpdatesLeaveExecutableIntact(t *testing.T) {
	for _, failure := range []string{"checksum", "gzip", "zip", "empty", "invalid-binary", "http", "truncated", "manifest"} {
		t.Run(failure, func(t *testing.T) {
			goos := "linux"
			if failure == "zip" {
				goos = "windows"
			}
			f := newFixture(t, goos)
			switch failure {
			case "checksum":
				f.archive = []byte("wrong bytes")
			case "gzip", "zip":
				f.archive = []byte("not an archive")
				f.manifest = fmt.Sprintf("%x%s", sha256.Sum256(f.archive), f.manifest[64:])
			case "empty":
				f.setRelease(t, "v1.10.0", "")
			case "invalid-binary":
				f.u.validate = validateBinary
			case "http":
				f.status = http.StatusServiceUnavailable
			case "truncated":
				f.archive = f.archive[:len(f.archive)-4]
				f.manifest = fmt.Sprintf("%x%s", sha256.Sum256(f.archive), f.manifest[64:])
			case "manifest":
				f.manifest = "broken"
			}
			if r, err := f.u.run(t.Context(), f.cfg); err == nil || r.Installed {
				t.Fatalf("bad update: %+v, %v", r, err)
			}
			assertContents(t, f.u.executable, "original")
			before := f.requests.Load()
			f.cfg.Install = false
			if _, err := f.u.run(t.Context(), f.cfg); err != nil {
				t.Fatal(err)
			}
			if f.requests.Load() != before {
				t.Fatal("failed check was not throttled")
			}
			// Fix the server and retry explicitly without waiting a day.
			f.status = http.StatusOK
			f.u.validate = func(string, string, string) error { return nil }
			f.setRelease(t, "v1.10.0", "replacement")
			f.cfg.Install = true
			if r, err := f.u.run(t.Context(), f.cfg); err != nil || !r.Installed {
				t.Fatalf("explicit retry was throttled: %+v, %v", r, err)
			}
		})
	}
}

func TestNeverDowngrade(t *testing.T) {
	f := newFixture(t, "linux")
	f.cfg.Version = "v2.0.0"
	r, err := f.u.run(t.Context(), f.cfg)
	if err != nil || r != (Result{}) || f.requests.Load() != 1 {
		t.Fatalf("downgrade: %+v, %v", r, err)
	}
	assertContents(t, f.u.executable, "original")
}

func TestConcurrentCheckDoesNotWaitOrDownloadTwice(t *testing.T) {
	f := newFixture(t, "linux")
	entered, release := make(chan struct{}), make(chan struct{})
	f.beforeDownload = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { _, err := f.u.run(t.Context(), f.cfg); done <- err }()
	<-entered
	r, err := f.u.run(t.Context(), f.cfg)
	close(release)
	firstErr := <-done
	if err == nil || firstErr != nil || r != (Result{}) || f.requests.Load() != 2 {
		t.Fatalf("parallel check: %+v, %v, %v, requests %d", r, err, firstErr, f.requests.Load())
	}
}

func TestExplicitUpdateReportsUnsupportedBuilds(t *testing.T) {
	for _, u := range []updater{{goos: "linux", goarch: "386"}, {goos: "plan9", goarch: "amd64"}} {
		if _, err := u.run(t.Context(), Config{Version: "v1.0.0", Install: true}); err == nil {
			t.Fatal("unsupported platform silently succeeded")
		}
	}
	if _, err := (&updater{}).run(t.Context(), Config{Version: "dev", Install: true}); err == nil {
		t.Fatal("development build silently succeeded")
	}
}

func TestExecutableChangedDuringDownload(t *testing.T) {
	f := newFixture(t, "linux")
	f.beforeDownload = func() {
		other := f.u.executable + ".other"
		if err := os.WriteFile(other, []byte("another installer"), 0o755); err != nil {
			t.Error(err)
			return
		}
		if _, err := replaceExecutable(f.u.executable, other); err != nil {
			t.Error(err)
		}
	}
	if r, err := f.u.run(t.Context(), f.cfg); err == nil || r.Installed {
		t.Fatalf("replaced another install: %+v, %v", r, err)
	}
	assertContents(t, f.u.executable, "another installer")
}

func TestFileIdentityIsCapturedBeforeAReplacement(t *testing.T) {
	f := newFixture(t, "linux")
	original, err := executableInfo(f.u.executable)
	if err != nil {
		t.Fatal(err)
	}
	other := f.u.executable + ".other"
	if err := os.WriteFile(other, []byte("original"), original.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, original.ModTime(), original.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := replaceExecutable(f.u.executable, other); err != nil {
		t.Fatal(err)
	}
	current, err := executableInfo(f.u.executable)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(original, current) {
		t.Fatal("file identity was obtained after replacement")
	}
}

func TestCancellationStopsNetworkWork(t *testing.T) {
	f := newFixture(t, "linux")
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer srv.Close()
	f.u.baseURL = srv.URL
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.u.run(ctx, f.cfg); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	assertContents(t, f.u.executable, "original")
}

func TestDownloadLimits(t *testing.T) {
	for _, body := range []string{"", "123456"} {
		if err := copyBounded(io.Discard, strings.NewReader(body), 5); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
	if err := copyBounded(io.Discard, strings.NewReader("12345"), 5); err != nil {
		t.Fatal(err)
	}
}

// This subprocess is a running executable, which is the important case on
// Windows. A test that only replaces an unopened text file would miss it.
func TestReplaceRunningExecutable(t *testing.T) {
	if os.Getenv("AZCP_TEST_SELFUPDATE") == "1" {
		target, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		stage := target + ".new"
		if err := os.WriteFile(stage, []byte("new executable"), 0o755); err != nil {
			t.Fatal(err)
		}
		backup, err := replaceExecutable(target, stage)
		if err != nil {
			t.Fatal(err)
		}
		assertContents(t, target, "new executable")
		if err := os.WriteFile(target+".backup", []byte(backup), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "azcp-test.exe")
	if err := os.WriteFile(target, b, 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command(target, "-test.run=^TestReplaceRunningExecutable$")
	c.Env = append(os.Environ(), "AZCP_TEST_SELFUPDATE=1")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("running replacement: %s, %v", out, err)
	}
	assertContents(t, target, "new executable")
	backup, err := os.ReadFile(target + ".backup")
	if err != nil {
		t.Fatal(err)
	}
	if len(backup) > 0 && !cleanupBackup(target, string(backup)) {
		t.Fatal("could not clean up after process exit")
	}
}

func TestFailedReplacementKeepsOriginal(t *testing.T) {
	f := newFixture(t, "linux")
	if _, err := replaceExecutable(f.u.executable, f.u.executable+".missing"); err == nil {
		t.Fatal("missing replacement succeeded")
	}
	assertContents(t, f.u.executable, "original")
}

func TestReleaseBinaryDownloadsAndRuns(t *testing.T) {
	// Exercise the same stripped executable and metadata as the release
	// workflow, through download, verification, replacement and --version.
	binary := filepath.Join(t.TempDir(), "release.exe")
	c := exec.Command("go", "build", "-trimpath", "-ldflags",
		"-s -w -X github.com/JohanLindvall/azcp/internal/cli.Version=v1.10.0",
		"-o", binary, "../../cmd/azcp")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build release: %s, %v", out, err)
	}
	if err := validateBinary(binary, runtime.GOOS, "wrong-architecture"); err == nil {
		t.Fatal("accepted an executable for the wrong architecture")
	}
	if err := validateBinary(binary, "wrong-os", runtime.GOARCH); err == nil {
		t.Fatal("accepted an executable for the wrong OS")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBinary(self, runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatal("accepted the test executable as azcp")
	}
	b, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, runtime.GOOS)
	f.u.goarch = runtime.GOARCH
	f.u.validate = validateBinary
	f.setRelease(t, "v1.10.0", string(b))
	r, err := f.u.run(t.Context(), f.cfg)
	if err != nil || !r.Installed {
		t.Fatalf("install release: %+v, %v", r, err)
	}
	c = exec.Command(f.u.executable, "--version")
	if out, err := c.CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "azcp v1.10.0\n") {
		t.Fatalf("installed release: %q, %v", out, err)
	}
}
