// SPDX-License-Identifier: MIT

package selfupdate

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/gzip"
)

const (
	maxManifest = 64 << 10
	maxArchive  = 128 << 20
	maxBinary   = 256 << 20
)

type release struct {
	version, asset string
	digest         [sha256.Size]byte
}

func assetSuffix(goos, goarch string) string {
	if goarch != "amd64" && goarch != "arm64" {
		return ""
	}
	ext := ".bin.gz"
	switch goos {
	case "linux", "darwin":
	case "windows":
		ext = ".exe.zip"
	default:
		return ""
	}
	return "_" + goos + "_" + goarch + ext
}

// The installer already uses this manifest. It identifies the release and its
// digest together, without spending GitHub's anonymous API request allowance.
func (u *updater) latest(ctx context.Context) (release, error) {
	var b strings.Builder
	if err := u.fetch(ctx, u.baseURL+"/latest/download/SHA256SUMS", &b, maxManifest); err != nil {
		return release{}, err
	}
	return parseManifest(b.String(), assetSuffix(u.goos, u.goarch))
}

func parseManifest(manifest, suffix string) (release, error) {
	var found release
	scan := bufio.NewScanner(strings.NewReader(manifest))
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) != 2 {
			return release{}, errors.New("invalid release checksum manifest")
		}
		asset := strings.TrimPrefix(fields[1], "*")
		if suffix == "" || !strings.HasPrefix(asset, "azcp_v") || !strings.HasSuffix(asset, suffix) {
			continue
		}
		version := strings.TrimSuffix(strings.TrimPrefix(asset, "azcp_"), suffix)
		if _, ok := releaseVersion(version); !ok {
			return release{}, errors.New("release manifest does not name a stable version")
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != sha256.Size || found.asset != "" {
			return release{}, errors.New("invalid or duplicate release checksum")
		}
		found.version, found.asset = version, asset
		copy(found.digest[:], digest)
	}
	if err := scan.Err(); err != nil {
		return release{}, err
	}
	if found.asset == "" {
		return release{}, errors.New("release has no binary for this platform")
	}
	return found, nil
}

func (u *updater) fetch(ctx context.Context, url string, dst io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "azcp-selfupdate")
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return errors.New("update download exceeds size limit")
	}
	return copyBounded(dst, resp.Body, limit)
}

func copyBounded(dst io.Writer, src io.Reader, limit int64) error {
	n, err := io.Copy(dst, io.LimitReader(src, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("update download exceeds size limit")
	}
	if n == 0 {
		return errors.New("empty update download")
	}
	return nil
}

func (u *updater) stage(ctx context.Context, r release, mode os.FileMode) (name string, err error) {
	archive, err := os.CreateTemp(u.cacheDir, "download-*")
	if err != nil {
		return "", err
	}
	defer func() { archive.Close(); os.Remove(archive.Name()) }()
	hash := sha256.New()
	if err := u.fetch(ctx, u.baseURL+"/download/"+r.version+"/"+r.asset,
		io.MultiWriter(archive, hash), maxArchive); err != nil {
		return "", err
	}
	if hex.EncodeToString(hash.Sum(nil)) != hex.EncodeToString(r.digest[:]) {
		return "", errors.New("update does not match its SHA-256 checksum")
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	var content io.ReadCloser
	if u.goos == "windows" {
		info, err := archive.Stat()
		if err != nil {
			return "", err
		}
		z, err := zip.NewReader(archive, info.Size())
		if err != nil {
			return "", err
		}
		if len(z.File) != 1 || z.File[0].Name != "azcp.exe" || !z.File[0].Mode().IsRegular() {
			return "", errors.New("update archive must contain only azcp.exe")
		}
		content, err = z.File[0].Open()
	} else {
		content, err = gzip.NewReader(archive)
	}
	if err != nil {
		return "", err
	}
	defer content.Close()
	// The replacement must be on the same filesystem as its final name.
	staged, err := os.CreateTemp(filepath.Dir(u.executable), ".azcp-new-*")
	if err != nil {
		return "", err
	}
	name = staged.Name()
	defer func() {
		staged.Close()
		if err != nil {
			os.Remove(name)
		}
	}()
	if err = copyBounded(staged, content, maxBinary); err != nil {
		return name, err
	}
	if err = staged.Sync(); err != nil {
		return name, err
	}
	if err = staged.Close(); err != nil {
		return name, err
	}
	if err = ctx.Err(); err != nil {
		return name, err
	}
	if err = u.validate(name, u.goos, u.goarch); err != nil {
		return name, err
	}
	err = os.Chmod(name, mode)
	return name, err
}

func validateBinary(path, goos, goarch string) error {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("invalid update executable: %w", err)
	}
	if info.Path != "github.com/JohanLindvall/azcp/cmd/azcp" {
		return errors.New("update executable is not azcp")
	}
	settings := make(map[string]string)
	for _, s := range info.Settings {
		settings[s.Key] = s.Value
	}
	if settings["GOOS"] != goos || settings["GOARCH"] != goarch {
		return errors.New("update executable is for another platform")
	}
	return nil
}
