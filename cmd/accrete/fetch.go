package main

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/hazame-hub/accretion/provider"
	"github.com/ulikunitz/xz"
)

// Fetching a disk image the provider does not have yet, so nobody uploads
// it by hand: download it on this machine, check its SHA-256 against the
// manifest's before anything is sent, decompress it, and upload it under the
// volume the build names. Nothing is uploaded from a file whose hash does
// not match.

// imageClient fetches images. A variable so a test can point it at a local
// server's TLS.
var imageClient = &http.Client{}

// ensureImage makes sure the provider has the recipe's image, fetching it
// when it does not and the manifest says where from. checked is true when
// this call downloaded it and its hash matched.
func (a *applier) ensureImage(rec provider.Recipe) (media provider.Media, checked bool, err error) {
	in := a.writer.(provider.Installer)
	req := rec.Image.Create
	media, err = in.Media(a.ctx, req.Placement, req.Image)
	var refused *provider.RefusedError
	if err == nil || rec.Image.URL == "" || !errors.As(err, &refused) {
		return media, false, err
	}
	up, ok := a.writer.(provider.ImageUploader)
	if !ok {
		return provider.Media{}, false, fmt.Errorf("%w; the provider cannot be handed an image, so upload it by hand", err)
	}
	if err := a.fetchImage(up, rec); err != nil {
		return provider.Media{}, false, err
	}
	media, err = in.Media(a.ctx, req.Placement, req.Image)
	if err != nil {
		return provider.Media{}, false, fmt.Errorf("uploaded, but the provider does not list it: %w", err)
	}
	return media, true, nil
}

func (a *applier) fetchImage(up provider.ImageUploader, rec provider.Recipe) error {
	from, want := rec.Image.URL, strings.ToLower(rec.Image.ImageSHA256)
	a.printf("  %s is not there yet; fetching %s\n", rec.Image.Create.Image, from)
	tmp, err := os.CreateTemp("", "accrete-image-*")
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()

	n, got, err := download(a.ctx, from, tmp)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", from, err)
	}
	if got != want {
		return fmt.Errorf("fetch %s: its SHA-256 is %s, not the declared %s; nothing was uploaded", from, got, want)
	}
	a.printf("  downloaded %d bytes, SHA-256 %s as declared\n", n, got)
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	body, err := decompressed(from, tmp)
	if err != nil {
		return fmt.Errorf("decompress %s: %w", from, err)
	}
	if err := up.UploadImage(a.ctx, rec.Image.Create.Placement, rec.Image.Create.Image, body); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	a.printf("  uploaded as %s\n", rec.Image.Create.Image)
	return nil
}

// download writes the body at from into w, returning its size and SHA-256.
func download(ctx context.Context, from string, w io.Writer) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, from, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := imageClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("the server answered %s", resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), resp.Body)
	if err != nil {
		return n, "", err
	}
	return n, fmt.Sprintf("%x", h.Sum(nil)), nil
}

// decompressed is r, decompressed when from names an .xz or .gz file.
func decompressed(from string, r io.Reader) (io.Reader, error) {
	path := from
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	switch {
	case strings.HasSuffix(path, ".xz"):
		return xz.NewReader(r)
	case strings.HasSuffix(path, ".gz"):
		return gzip.NewReader(r)
	}
	return r, nil
}
