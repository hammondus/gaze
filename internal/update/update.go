// Package update replaces the running executable with the latest release.
//
// It uses no API. The version comes from the redirect that
// /releases/latest issues to /releases/tag/<version>, and the binary comes
// from the /releases/latest/download/<asset> path. Both are plain web requests,
// so the documented limit of 60 unauthenticated REST API calls per hour per IP
// address does not apply.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Repo is where releases are published.
const Repo = "https://github.com/hammondus/gaze"

// maxAsset caps a download. The binary is under 10 MB; anything an order of
// magnitude larger means the URL is not what this code thinks it is, and
// writing it to disk would be the wrong move.
const maxAsset = 100 << 20

// timeout bounds the whole exchange, download included.
const timeout = 2 * time.Minute

// Updater replaces one executable with the newest published build.
type Updater struct {
	// Repo is the project's GitHub URL. Tests point it at a local server.
	Repo string
	// Version is the running build, as reported by --version.
	Version string
	// Asset is the release asset for this machine.
	Asset string
	// Target is the file to replace.
	Target string
	// Client is used for every request. A nil Client gets a default one.
	Client *http.Client
}

// New returns an Updater for the running executable.
//
// The target is resolved through any symlinks, so an update through a
// /usr/local/bin symlink replaces the real file rather than the link.
func New(version string) (*Updater, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("gaze only publishes Linux builds, and this is %s", runtime.GOOS)
	}
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return nil, fmt.Errorf("no published build for %s", arch)
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot find the running executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	return &Updater{
		Repo:    Repo,
		Version: version,
		Asset:   "gaze-linux-" + arch,
		Target:  exe,
	}, nil
}

// client returns the HTTP client to use.
func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: timeout}
}

// userAgent identifies this program to GitHub, with somewhere to complain to.
func (u *Updater) userAgent() string {
	return "gaze/" + u.Version + " (+" + Repo + ")"
}

// get issues a GET with the identifying User-Agent.
func (u *Updater) get(c *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", u.userAgent())
	return c.Do(req)
}

// Latest returns the version tag of the newest release.
//
// /releases/latest answers with a redirect to /releases/tag/<version>, so the
// tag is read from the Location header and the response body is never
// fetched. Redirects are deliberately not followed: following one would
// download the whole release page to learn something already in the header.
func (u *Updater) Latest() (string, error) {
	c := *u.client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	resp, err := u.get(&c, u.Repo+"/releases/latest")
	if err != nil {
		return "", fmt.Errorf("cannot reach %s: %w", u.Repo, err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
		return "", fmt.Errorf("no release found: %s answered %s", u.Repo+"/releases/latest", resp.Status)
	}
	// The header names the tag page, so the tag is its last path element.
	tag := path.Base(loc)
	if tag == "" || tag == "." || tag == "/" || tag == "latest" {
		return "", fmt.Errorf("cannot read a version from %q", loc)
	}
	return tag, nil
}

// Available reports the latest version and whether it differs from the running
// build.
//
// Versions are compared for equality, not ordered. Ordering would mean parsing
// semantic versions, and the only question worth asking is whether the running
// build is the published one: a "dev" build never is, and that is worth saying.
func (u *Updater) Available() (latest string, differs bool, err error) {
	latest, err = u.Latest()
	if err != nil {
		return "", false, err
	}
	return latest, latest != u.Version, nil
}

// Apply downloads the newest release and replaces the target with it,
// reporting progress to w.
func (u *Updater) Apply(w io.Writer) error {
	latest, differs, err := u.Available()
	if err != nil {
		return err
	}
	if !differs {
		fmt.Fprintf(w, "gaze %s is the published version; nothing to do\n", latest)
		return nil
	}
	fmt.Fprintf(w, "updating gaze %s to %s\n", u.Version, latest)

	// Fail on an unwritable directory before spending a download on it. The
	// replacement is a create-and-rename inside the target's directory, so
	// that directory is what must be writable, not the file.
	dir := filepath.Dir(u.Target)
	if err := checkWritable(dir); err != nil {
		return fmt.Errorf("cannot write to %s: %w\ntry: sudo %s --update", dir, err, u.Target)
	}

	want, err := u.wantedSum()
	if err != nil {
		return err
	}

	tmp, sum, err := u.download(dir)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // harmless once the rename below has moved it

	if sum != want {
		return fmt.Errorf("checksum mismatch for %s:\n  published %s\n  received  %s",
			u.Asset, want, sum)
	}
	fmt.Fprintf(w, "verified sha256 %s\n", sum[:16])

	// Rename is atomic within one filesystem, and Linux keeps the running
	// program's own inode alive, so replacing the file underneath a running
	// gaze is safe.
	if err := os.Rename(tmp, u.Target); err != nil {
		return fmt.Errorf("cannot replace %s: %w", u.Target, err)
	}
	fmt.Fprintf(w, "installed %s to %s\n", latest, u.Target)
	return nil
}

// wantedSum fetches SHA256SUMS and returns the line for this machine's asset.
//
// The checksum and the binary come from the same server, so this proves the
// download arrived intact. It is not a signature and does not prove who built
// it; see DESIGN-DECISIONS.md.
func (u *Updater) wantedSum() (string, error) {
	resp, err := u.get(u.client(), u.Repo+"/releases/latest/download/SHA256SUMS")
	if err != nil {
		return "", fmt.Errorf("cannot fetch SHA256SUMS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cannot fetch SHA256SUMS: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return sumFor(string(body), u.Asset)
}

// sumFor picks one asset's checksum out of a SHA256SUMS file, whose lines are
// a hex digest and a file name.
func sumFor(body, asset string) (string, error) {
	for _, line := range strings.Split(body, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == asset {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS lists no entry for %s", asset)
}

// download streams the release asset into a new file beside the target and
// returns the file's path and its checksum.
//
// The file is written next to the target rather than in a temporary directory,
// because a rename only works within one filesystem and /tmp is often a
// separate one.
func (u *Updater) download(dir string) (string, string, error) {
	resp, err := u.get(u.client(), u.Repo+"/releases/latest/download/"+u.Asset)
	if err != nil {
		return "", "", fmt.Errorf("cannot download %s: %w", u.Asset, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("cannot download %s: %s", u.Asset, resp.Status)
	}

	f, err := os.CreateTemp(dir, ".gaze-update-*")
	if err != nil {
		return "", "", err
	}
	tmp := f.Name()
	fail := func(err error) (string, string, error) {
		f.Close()
		os.Remove(tmp)
		return "", "", err
	}

	// The mode must be set before the rename, or the installed file lands
	// with the 0600 that CreateTemp gives it and stops being runnable.
	if err := f.Chmod(0o755); err != nil {
		return fail(err)
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAsset))
	if err != nil {
		return fail(fmt.Errorf("download failed after %d bytes: %w", n, err))
	}
	if n == maxAsset {
		return fail(fmt.Errorf("%s is larger than the %d byte limit", u.Asset, int64(maxAsset)))
	}
	if n == 0 {
		return fail(errors.New("the download was empty"))
	}
	// Flush to disk before the rename, so a crash cannot leave a truncated
	// binary at the target path.
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	return tmp, hex.EncodeToString(h.Sum(nil)), nil
}

// checkWritable reports whether a directory accepts new files.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".gaze-writable-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}
