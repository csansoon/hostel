package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// releaseKey is the public key of the release signatures (the private key is
// the repository secret HOSTEL_RELEASE_KEY; owner decision, 2026-11-04).
const releaseKey = "IK0EN/qS1cs9mgsUyQF2M1XtRX/BX3RnJmujfts+Iec="

const (
	defaultBase = "https://github.com/csansoon/hostel/releases"
	// tagTimeout bounds the read of the latest tag and of SHA256SUMS;
	// fileTimeout one binary (mise is about 150 MB).
	tagTimeout  = 30 * time.Second
	fileTimeout = 10 * time.Minute
)

// ErrUnreachable marks a release source that does not answer.
var ErrUnreachable = errors.New("the release source cannot be reached")

// Source is where releases are: GitHub, or the mirror of HOSTEL_UPDATE_URL
// (with its key HOSTEL_UPDATE_KEY; the E2E tests use both).
type Source struct {
	Base string
	Key  ed25519.PublicKey
	http *http.Client
}

// Default is the release source of this computer.
func Default() (*Source, error) {
	base, key := defaultBase, releaseKey
	if u := os.Getenv("HOSTEL_UPDATE_URL"); u != "" {
		base = strings.TrimSuffix(u, "/")
		if k := os.Getenv("HOSTEL_UPDATE_KEY"); k != "" {
			key = k
		}
	}
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("the release key is not an Ed25519 public key in base64")
	}
	return &Source{Base: base, Key: raw, http: &http.Client{
		// The latest tag is the redirect itself.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if strings.HasSuffix(via[0].URL.Path, "/latest") {
				return http.ErrUseLastResponse
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}}, nil
}

// Latest returns the tag of the latest release: the redirect of
// <base>/latest to …/tag/<tag>.
func (s *Source) Latest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, tagTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrUnreachable, s.Base, err)
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
		return "", fmt.Errorf("%w: %s/latest: HTTP %d without a redirect to the release", ErrUnreachable, s.Base, resp.StatusCode)
	}
	tag := path.Base(strings.TrimSuffix(loc, "/"))
	if _, ok := ParseTag(tag); !ok {
		return "", fmt.Errorf("the latest release has the tag %q, not vX.Y.Z", tag)
	}
	return tag, nil
}

// Release is a verified SHA256SUMS: file name to SHA-256 (hex).
type Release struct {
	Tag  string
	Sums map[string]string
}

// SignedText is the text that a release signature covers: the tag and
// SHA256SUMS. The tag stops an old release that is served under a new tag.
func SignedText(tag string, sums []byte) []byte {
	return append([]byte("hostel-release "+tag+"\n"), sums...)
}

// Release reads SHA256SUMS and SHA256SUMS.sig of tag and checks the signature.
func (s *Source) Release(ctx context.Context, tag string) (*Release, error) {
	if _, ok := ParseTag(tag); !ok {
		return nil, fmt.Errorf("the version %q is not vX.Y.Z", tag)
	}
	sums, err := s.small(ctx, tag, "SHA256SUMS")
	if err != nil {
		return nil, err
	}
	sig, err := s.small(ctx, tag, "SHA256SUMS.sig")
	if err != nil {
		return nil, fmt.Errorf("%w (a release before v0.2.0 has no signature: install it with the install command)", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(s.Key, SignedText(tag, sums), raw) {
		return nil, fmt.Errorf("the signature of release %s is not valid: do not install it, and tell the Hostel maintainers", tag)
	}
	r := &Release{Tag: tag, Sums: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 {
			r.Sums[f[1]] = f[0]
		}
	}
	return r, nil
}

func (s *Source) url(tag, file string) string { return s.Base + "/download/" + tag + "/" + file }

// small reads a small release file (at most 1 MB).
func (s *Source) small(ctx context.Context, tag, file string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, tagTimeout)
	defer cancel()
	resp, err := s.get(ctx, tag, file)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrUnreachable, s.url(tag, file), err)
	}
	return data, nil
}

func (s *Source) get(ctx context.Context, tag, file string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url(tag, file), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnreachable, s.url(tag, file), err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download %s: HTTP %d", s.url(tag, file), resp.StatusCode)
	}
	return resp, nil
}

// Download writes the release file to a new temporary file next to dst (the
// same file system, so that a rename replaces dst), checks its SHA-256
// against r, and returns its path. The file is removed on an error.
func (s *Source) Download(ctx context.Context, r *Release, file, dst string) (tmp string, err error) {
	want, ok := r.Sums[file]
	if !ok {
		return "", fmt.Errorf("release %s has no file %s", r.Tag, file)
	}
	ctx, cancel := context.WithTimeout(ctx, fileTimeout)
	defer cancel()
	resp, err := s.get(ctx, r.Tag, file)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	f, err := TempNext(dst)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return "", fmt.Errorf("%w: download %s: %v", ErrUnreachable, file, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("the checksum of %s is not the one of release %s: do not install it", file, r.Tag)
	}
	return f.Name(), os.Chmod(f.Name(), 0o755)
}

// FileSHA256 is the SHA-256 (hex) of a file ("" when it is missing).
func FileSHA256(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TempNext creates a new temporary file next to file (a unique name, never
// an existing file or link).
func TempNext(file string) (*os.File, error) {
	return os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".new-*")
}
