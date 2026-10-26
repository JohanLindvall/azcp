// SPDX-License-Identifier: MIT

// Package selfupdate checks the published release checksums and replaces the
// executable only after the matching binary has been downloaded and verified.
package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	releasesURL    = "https://github.com/JohanLindvall/azcp/releases"
	checkInterval  = 24 * time.Hour
	checkTimeout   = 5 * time.Second
	installTimeout = time.Minute
)

// Config is supplied by the CLI, whose defaults own the opt-in policy.
type Config struct {
	Version      string
	VersionCheck bool
	Install      bool // an explicit request, never an automatic copy side effect
}

// Result is separate from the copy's result: maintenance can never change the
// command's exit status or add events to its JSON output.
type Result struct {
	Version   string
	Installed bool
}

type updater struct {
	client       *http.Client
	baseURL      string
	goos, goarch string
	executable   string
	cacheDir     string
	now          func() time.Time
	// Tests exercise the complete download path without needing a release
	// executable for every platform. Production verifies the Go build info.
	validate func(string, string, string) error
}

type state struct {
	Checked time.Time
	Current string
	Backup  string
}

// Run silently skips automatic checks in containers and development builds;
// an explicit installation reports why it cannot proceed instead.
func Run(ctx context.Context, cfg Config) (Result, error) {
	if !cfg.VersionCheck && !cfg.Install {
		return Result{}, nil
	}
	if InContainer() {
		if cfg.Install {
			return Result{}, errors.New("self-updates are disabled in containers; pull a newer image")
		}
		return Result{}, nil
	}
	if _, ok := releaseVersion(cfg.Version); !ok {
		if cfg.Install {
			return Result{}, fmt.Errorf("self-updates require a stable release (running %s)", cfg.Version)
		}
		return Result{}, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return Result{}, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return Result{}, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return Result{}, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("update download redirected away from HTTPS")
		}
		if len(via) >= 10 {
			return errors.New("too many update redirects")
		}
		return nil
	}}
	u := updater{client: client, baseURL: releasesURL, goos: runtime.GOOS, goarch: runtime.GOARCH,
		executable: exe, cacheDir: filepath.Join(cache, "azcp", "updates"), now: time.Now, validate: validateBinary}
	return u.run(ctx, cfg)
}

func (u *updater) run(ctx context.Context, cfg Config) (result Result, err error) {
	if !cfg.VersionCheck && !cfg.Install {
		return result, nil
	}
	if _, ok := releaseVersion(cfg.Version); !ok {
		if cfg.Install {
			return result, fmt.Errorf("self-updates require a stable release (running %s)", cfg.Version)
		}
		return result, nil
	}
	if assetSuffix(u.goos, u.goarch) == "" {
		if cfg.Install {
			return result, fmt.Errorf("no release binary for %s/%s", u.goos, u.goarch)
		}
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	original, err := executableInfo(u.executable)
	if err != nil {
		return result, err
	}
	if !original.Mode().IsRegular() || original.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return result, errors.New("cannot update a special or set-ID executable")
	}
	if err := os.MkdirAll(u.cacheDir, 0o700); err != nil {
		return result, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(u.executable)))
	path := filepath.Join(u.cacheDir, key+".json")
	unlock, err := lock(path + ".lock")
	if err != nil || unlock == nil {
		if err == nil && cfg.Install {
			err = errors.New("another version check or update is in progress; try again")
		}
		return result, err
	}
	defer unlock()
	s := state{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	if s.Backup != "" && cleanupBackup(u.executable, s.Backup) {
		s.Backup = ""
		if err := saveState(path, s); err != nil {
			return result, err
		}
	}
	now := u.now()
	if !s.Checked.After(now) && now.Sub(s.Checked) < checkInterval {
		// A process started before another invocation installed an update can
		// still be running the old version. It must not reinstall or downgrade
		// the executable the other invocation has just put in place.
		if newer(s.Current, cfg.Version) {
			if cfg.Install {
				return result, errors.New("another process updated this executable; run the command again")
			}
			return result, nil
		}
		if !cfg.Install && s.Current == cfg.Version {
			return result, nil
		}
	}
	// Remember even a refused request: offline invocations and a rate limit
	// should not make every subsequent cp wait for the network again.
	s.Checked, s.Current = now, cfg.Version
	if err := saveState(path, s); err != nil {
		return result, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	r, err := u.latest(checkCtx)
	cancel()
	if err != nil || !newer(r.version, cfg.Version) {
		return result, err
	}
	result.Version = r.version
	if !cfg.Install {
		return result, nil
	}
	installCtx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	staged, err := u.stage(installCtx, r, original.Mode().Perm())
	if err != nil {
		return result, err
	}
	defer os.Remove(staged)
	if err := installCtx.Err(); err != nil {
		return result, err
	}
	// Another installer may have replaced this pathname while we downloaded.
	// Never replace its work based on the version of our still-running inode.
	current, err := executableInfo(u.executable)
	if err != nil {
		return result, err
	}
	if !os.SameFile(original, current) || original.Size() != current.Size() || !original.ModTime().Equal(current.ModTime()) {
		return result, errors.New("executable changed while downloading the update")
	}
	backup, err := replaceExecutable(u.executable, staged)
	if err != nil {
		return result, err
	}
	result.Installed = true
	s.Current, s.Backup = r.version, backup
	return result, saveState(path, s)
}

func executableInfo(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("executable is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// Windows pathname Stat defers fetching the file ID until SameFile is
	// called. File.Stat captures it now, before a replacement can occur.
	return f.Stat()
}

func saveState(path string, s state) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// The separate OS lock is held while reading and writing this file. An
	// interrupted write only loses a throttle timestamp, never an executable.
	return os.WriteFile(path, b, 0o600)
}
