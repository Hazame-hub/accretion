package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"time"
)

// accrete image fedora-coreos: the inputs that pin a build to the current
// Fedora CoreOS proxmoxve image, read from Fedora's stream metadata. It
// changes nothing; paste its lines into a resource's inputs.

const imageUsage = `Usage:
  accrete image fedora-coreos [--stream stable|testing|next] [--storage local] [--arch x86_64]

Prints the image, image_url and image_sha256 inputs of image-proxmox-vm for
the current Fedora CoreOS release of the stream, from Fedora's stream
metadata. apply then fetches the image itself when the storage lacks it,
checks its SHA-256 and uploads it: nobody uploads it by hand.
`

// fcosStreams is where Fedora publishes its stream metadata. A variable so a
// test can serve its own.
var fcosStreams = "https://builds.coreos.fedoraproject.org/streams/"

var streamName = regexp.MustCompile(`^(stable|testing|next)$`)

func runImage(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "fedora-coreos" {
		_, _ = fmt.Fprint(stderr, imageUsage)
		return 2
	}
	fs := flag.NewFlagSet("accrete image fedora-coreos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stream := fs.String("stream", "stable", "the Fedora CoreOS stream: stable, testing or next")
	storage := fs.String("storage", "local", "the storage whose import content holds the image")
	arch := fs.String("arch", "x86_64", "the architecture")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 || !streamName.MatchString(*stream) || !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`).MatchString(*storage) {
		_, _ = fmt.Fprint(stderr, imageUsage)
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fcosStreams+*stream+".json", nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "accrete: %v\n", err)
		return 2
	}
	resp, err := imageClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "accrete: read the %s stream: %v\n", *stream, err)
		return 2
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(stderr, "accrete: read the %s stream: the server answered %s\n", *stream, resp.Status)
		return 2
	}
	var doc struct {
		Architectures map[string]struct {
			Artifacts map[string]struct {
				Release string `json:"release"`
				Formats map[string]struct {
					Disk struct {
						Location           string `json:"location"`
						SHA256             string `json:"sha256"`
						UncompressedSHA256 string `json:"uncompressed-sha256"`
					} `json:"disk"`
				} `json:"formats"`
			} `json:"artifacts"`
		} `json:"architectures"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		_, _ = fmt.Fprintf(stderr, "accrete: read the %s stream: %v\n", *stream, err)
		return 2
	}
	art, ok := doc.Architectures[*arch].Artifacts["proxmoxve"]
	disk := art.Formats["qcow2.xz"].Disk
	if !ok || disk.Location == "" || !sha256Hex.MatchString(disk.SHA256) {
		_, _ = fmt.Fprintf(stderr, "accrete: the %s stream lists no proxmoxve qcow2.xz image for %s\n", *stream, *arch)
		return 1
	}
	name := path.Base(disk.Location)
	if len(name) > 3 && name[len(name)-3:] == ".xz" {
		name = name[:len(name)-3]
	}
	_, _ = fmt.Fprintf(stdout, "# Fedora CoreOS %s %s (%s), proxmoxve\n", *stream, art.Release, *arch)
	_, _ = fmt.Fprintf(stdout, "        image: %s:import/%s\n", *storage, name)
	_, _ = fmt.Fprintf(stdout, "        image_url: %s\n", disk.Location)
	_, _ = fmt.Fprintf(stdout, "        image_sha256: %s\n", disk.SHA256)
	_, _ = fmt.Fprintf(stdout, "        seed_format: ignition\n")
	return 0
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)
