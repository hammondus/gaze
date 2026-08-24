package update

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRelease serves the two paths the updater uses: the redirect that names
// the latest version, and the download path for an asset.
//
// Standing a local server up means the whole flow is tested — redirect
// handling, checksum verification, the atomic replace — without a single
// request to GitHub.
type fakeRelease struct {
	tag     string
	asset   string
	body    []byte
	sums    string // overrides the generated SHA256SUMS when set
	noTag   bool   // answer /releases/latest with 404
	srv     *httptest.Server
	hitPath []string
}

func newFakeRelease(t *testing.T, tag, asset string, body []byte) *fakeRelease {
	t.Helper()
	f := &fakeRelease{tag: tag, asset: asset, body: body}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.hitPath = append(f.hitPath, r.URL.Path)
		if f.noTag {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/releases/tag/"+f.tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/latest/download/", func(w http.ResponseWriter, r *http.Request) {
		f.hitPath = append(f.hitPath, r.URL.Path)
		switch filepath.Base(r.URL.Path) {
		case "SHA256SUMS":
			w.Write([]byte(f.sumsFile()))
		case f.asset:
			w.Write(f.body)
		default:
			http.NotFound(w, r)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRelease) sumsFile() string {
	if f.sums != "" {
		return f.sums
	}
	sum := sha256.Sum256(f.body)
	return fmt.Sprintf("%s  other-asset\n%s  %s\n",
		strings.Repeat("0", 64), hex.EncodeToString(sum[:]), f.asset)
}

// updaterFor returns an Updater pointed at the fake release, with a target
// file standing in for the installed binary.
func updaterFor(t *testing.T, f *fakeRelease, version string) (*Updater, string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "gaze")
	if err := os.WriteFile(target, []byte("the old build"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Updater{
		Repo:    f.srv.URL,
		Version: version,
		Asset:   f.asset,
		Target:  target,
	}, target
}

func TestLatestReadsTheRedirect(t *testing.T) {
	f := newFakeRelease(t, "v1.2.3", "gaze-linux-arm64", []byte("binary"))
	u, _ := updaterFor(t, f, "v1.0.0")

	got, err := u.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1.2.3" {
		t.Errorf("latest = %q, want v1.2.3", got)
	}
	// The redirect must not be followed: the tag is in the header, and
	// following it would fetch the whole release page for nothing.
	for _, p := range f.hitPath {
		if strings.Contains(p, "/releases/tag/") {
			t.Errorf("the updater followed the redirect to %s", p)
		}
	}
}

func TestLatestWithoutAnyRelease(t *testing.T) {
	f := newFakeRelease(t, "v1.0.0", "gaze-linux-arm64", []byte("binary"))
	f.noTag = true
	u, _ := updaterFor(t, f, "v1.0.0")

	if _, err := u.Latest(); err == nil {
		t.Error("want an error when no release exists")
	}
}

func TestAvailable(t *testing.T) {
	f := newFakeRelease(t, "v2.0.0", "gaze-linux-arm64", []byte("binary"))

	u, _ := updaterFor(t, f, "v1.0.0")
	if latest, differs, err := u.Available(); err != nil || latest != "v2.0.0" || !differs {
		t.Errorf("older build: got %q %v %v", latest, differs, err)
	}

	u, _ = updaterFor(t, f, "v2.0.0")
	if _, differs, _ := u.Available(); differs {
		t.Error("a build matching the release must not report an update")
	}

	// A build with no version stamp is never the published one, and saying so
	// is more useful than pretending it is up to date.
	u, _ = updaterFor(t, f, "dev")
	if _, differs, _ := u.Available(); !differs {
		t.Error("a dev build should report that a release is available")
	}
}

func TestApplyReplacesTheBinary(t *testing.T) {
	want := []byte("the new build, much improved")
	f := newFakeRelease(t, "v2.0.0", "gaze-linux-arm64", want)
	u, target := updaterFor(t, f, "v1.0.0")

	var out strings.Builder
	if err := u.Apply(&out); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("installed %q, want %q", got, want)
	}
	// CreateTemp makes a 0600 file. Without an explicit chmod the installed
	// binary would not be runnable.
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want -rwxr-xr-x", fi.Mode().Perm())
	}
	for _, want := range []string{"v1.0.0 to v2.0.0", "verified sha256", "installed v2.0.0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not mention %q:\n%s", want, out.String())
		}
	}
	// Nothing should be left behind beside the target.
	leftovers := tempLeftovers(t, filepath.Dir(target))
	if len(leftovers) > 0 {
		t.Errorf("left temporary files behind: %v", leftovers)
	}
}

func TestApplyOnCurrentVersionDoesNothing(t *testing.T) {
	f := newFakeRelease(t, "v2.0.0", "gaze-linux-arm64", []byte("new"))
	u, target := updaterFor(t, f, "v2.0.0")

	var out strings.Builder
	if err := u.Apply(&out); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "the old build" {
		t.Errorf("the binary was replaced when it was already current: %q", got)
	}
	if !strings.Contains(out.String(), "nothing to do") {
		t.Errorf("output = %q", out.String())
	}
}

// TestApplyRejectsABadChecksum is the test that matters most: a download that
// does not match the published checksum must never reach the target path.
func TestApplyRejectsABadChecksum(t *testing.T) {
	f := newFakeRelease(t, "v2.0.0", "gaze-linux-arm64", []byte("tampered payload"))
	f.sums = strings.Repeat("a", 64) + "  gaze-linux-arm64\n"
	u, target := updaterFor(t, f, "v1.0.0")

	err := u.Apply(&strings.Builder{})
	if err == nil {
		t.Fatal("want an error for a checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "the old build" {
		t.Errorf("the target was replaced despite a bad checksum: %q", got)
	}
	if l := tempLeftovers(t, filepath.Dir(target)); len(l) > 0 {
		t.Errorf("a rejected download was left on disk: %v", l)
	}
}

func TestApplyWithoutAChecksumEntry(t *testing.T) {
	f := newFakeRelease(t, "v2.0.0", "gaze-linux-arm64", []byte("new"))
	f.sums = strings.Repeat("b", 64) + "  gaze-linux-riscv64\n"
	u, target := updaterFor(t, f, "v1.0.0")

	err := u.Apply(&strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "no entry for") {
		t.Fatalf("want a missing-entry error, got %v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != "the old build" {
		t.Error("the target was replaced without a published checksum")
	}
}

// TestApplyOnUnwritableDirectory checks the message names the fix. Root-owned
// /usr/local/bin is the normal case, and a bare permissions error would leave
// the user guessing.
func TestApplyOnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	f := newFakeRelease(t, "v2.0.0", "gaze-linux-arm64", []byte("new"))
	u, target := updaterFor(t, f, "v1.0.0")

	dir := filepath.Dir(target)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	err := u.Apply(&strings.Builder{})
	if err == nil {
		t.Fatal("want an error for an unwritable directory")
	}
	if !strings.Contains(err.Error(), "sudo") {
		t.Errorf("the error should suggest sudo: %v", err)
	}
}

func TestSumFor(t *testing.T) {
	body := "aaa  gaze-linux-amd64\nbbb  gaze-linux-arm64\nccc  SHA256SUMS\n"
	if got, err := sumFor(body, "gaze-linux-arm64"); err != nil || got != "bbb" {
		t.Errorf("sumFor = %q, %v", got, err)
	}
	if _, err := sumFor(body, "gaze-linux-riscv64"); err == nil {
		t.Error("want an error for an asset that is not listed")
	}
	// A partial name must not match: gaze-linux-arm64 is a prefix of nothing
	// here, but a substring match would be a real hazard as assets are added.
	if _, err := sumFor("bbb  gaze-linux-arm64-debug\n", "gaze-linux-arm64"); err == nil {
		t.Error("a longer asset name must not match a shorter request")
	}
}

// TestUserAgentIdentifiesTheClient checks the header, since anything that
// polls someone else's service should say who it is and where to complain.
func TestUserAgentIdentifiesTheClient(t *testing.T) {
	u := &Updater{Version: "v1.2.3"}
	got := u.userAgent()
	if !strings.Contains(got, "gaze/v1.2.3") || !strings.Contains(got, "github.com/hammondus/gaze") {
		t.Errorf("user agent = %q", got)
	}
}

// tempLeftovers lists the updater's temporary files in a directory.
func tempLeftovers(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".gaze-") {
			out = append(out, e.Name())
		}
	}
	return out
}
