package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ulikunitz/xz"
)

// An image the storage does not have is fetched by apply: downloaded,
// checked against image_sha256, decompressed and uploaded, so nobody
// uploads it by hand.

func xzOf(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func imageServer(t *testing.T, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasSuffix(r.URL.Path, ".qcow2.xz") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func fetchLab(t *testing.T, url, sum string) *applyLab {
	t.Helper()
	l := newImageLab(t, imageManifest, "        image_url: "+url+"\n        image_sha256: "+sum+"\n")
	delete(l.pve.Files, fcosImage)
	return l
}

func TestApplyFetchesAnImageTheStorageLacks(t *testing.T) {
	qcow := []byte("QFI\xfb the decompressed Fedora CoreOS image")
	packed := xzOf(t, qcow)
	srv, _ := imageServer(t, packed)
	url := srv.URL + "/fedora-coreos-proxmoxve.x86_64.qcow2.xz"
	sum := fmt.Sprintf("%x", sha256.Sum256(packed))
	l := fetchLab(t, url, sum)

	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, fcosImage+" is not there yet; fetching "+url,
		fmt.Sprintf("downloaded %d bytes, SHA-256 %s as declared", len(packed), sum),
		"uploaded as "+fcosImage, "fetched from "+url+" with SHA-256 "+sum+" (checked on download)", "confirmed and recorded")
	if got := l.pve.Files[fcosImage]; !bytes.Equal(got, qcow) {
		t.Errorf("uploaded %q, want the decompressed image", got)
	}
	s := l.loadState()
	rec := s.Images[current(s)[0].ID]
	if rec.ImageSHA256Fetched != sum || rec.ImageSHA256Declared != "" || rec.ImageURL != url {
		t.Errorf("image record %+v", rec)
	}

	// A second machine finds the image there and fetches nothing.
	l2 := newImageLab(t, imageManifest, "        image_url: "+url+"\n        image_sha256: "+sum+"\n")
	out, code = l2.accrete("", "apply", "--auto-approve")
	if code != 0 || strings.Contains(out, "fetching") {
		t.Fatalf("with the image there: exit %d\n%s", code, out)
	}
	mustContain(t, out, "SHA-256 "+sum+" as declared (not checked)")
}

func TestAFetchedImageWhoseHashDiffersIsNeverUploaded(t *testing.T) {
	srv, _ := imageServer(t, xzOf(t, []byte("tampered")))
	l := fetchLab(t, srv.URL+"/fcos.qcow2.xz", strings.Repeat("ab", 32))
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "not the declared "+strings.Repeat("ab", 32)+"; nothing was uploaded")
	if _, there := l.pve.Files[fcosImage]; there {
		t.Error("the image was uploaded")
	}
	for _, w := range l.pve.Writes() {
		if strings.Contains(w, "/upload") || strings.Contains(w, "POST /nodes/pve1/qemu") {
			t.Errorf("write after a bad hash: %s", w)
		}
	}
}

func TestAStorageWithoutImportContentIsNamed(t *testing.T) {
	packed := xzOf(t, []byte("img"))
	srv, hits := imageServer(t, packed)
	l := fetchLab(t, srv.URL+"/fcos.qcow2.xz", fmt.Sprintf("%x", sha256.Sum256(packed)))
	l.pve.NoImportContent = true
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "storage local on pve1 does not take disk images; add the Import content to it once (Datacenter → Storage → local → Edit → Content)")
	if hits.Load() != 1 {
		t.Errorf("downloads: %d", hits.Load())
	}
}

func TestAnImageURLNeedsItsHash(t *testing.T) {
	l := newImageLab(t, imageManifest, "        image_url: https://example.invalid/fcos.qcow2.xz\n")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code == 0 || !strings.Contains(out, "a fetched image needs image_sha256") || len(l.pve.Writes()) != 0 {
		t.Fatalf("exit %d, writes %v\n%s", code, l.pve.Writes(), out)
	}
}

func TestPermissionsForAFetchedImageIncludeTheUpload(t *testing.T) {
	l := newImageLab(t, imageManifest, "        image_url: https://example.invalid/fcos.qcow2.xz\n        image_sha256: "+strings.Repeat("ab", 32)+"\n")
	out, _ := l.accrete("", "permissions", "--offline")
	mustContain(t, out, "Datastore.AllocateTemplate", "upload the disk image accrete fetches")
}

const testStream = `{"stream":"stable","architectures":{"x86_64":{"artifacts":{"proxmoxve":{"release":"44.20261023.3.1","formats":{"qcow2.xz":{"disk":{
"location":"https://builds.example/prod/streams/stable/builds/44.20261023.3.1/x86_64/fedora-coreos-44.20261023.3.1-proxmoxve.x86_64.qcow2.xz",
"sha256":"8182e2b95824b4e6c42eaaab791685bbfcb1b704fe55758aeff0a2a14a319648","uncompressed-sha256":"88fb3591c346c87ff3f385749d7a505a3c41d5e948949259a406ec8873d91378"}}}}}}}}`

func TestImageFedoraCoreOSPrintsThePinnedInputs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/streams/stable.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(testStream))
	}))
	defer srv.Close()
	old := fcosStreams
	fcosStreams = srv.URL + "/streams/"
	defer func() { fcosStreams = old }()

	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"image", "fedora-coreos", "--storage", "vm-images"}, nil, &out, &errb, func(string) string { return "" }); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := "# Fedora CoreOS stable 44.20261023.3.1 (x86_64), proxmoxve\n" +
		"        image: vm-images:import/fedora-coreos-44.20261023.3.1-proxmoxve.x86_64.qcow2\n" +
		"        image_url: https://builds.example/prod/streams/stable/builds/44.20261023.3.1/x86_64/fedora-coreos-44.20261023.3.1-proxmoxve.x86_64.qcow2.xz\n" +
		"        image_sha256: 8182e2b95824b4e6c42eaaab791685bbfcb1b704fe55758aeff0a2a14a319648\n" +
		"        seed_format: ignition\n"
	if out.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", out.String(), want)
	}
	if code := run(t.Context(), []string{"image", "fedora-coreos", "--stream", "rawhide"}, nil, &out, &errb, func(string) string { return "" }); code != 2 {
		t.Errorf("a bad stream: exit %d", code)
	}
}
