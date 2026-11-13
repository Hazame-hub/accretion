package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hazame-hub/accretion/bootstrap"
	"github.com/hazame-hub/accretion/provider"
)

// A build from a disk image: any OS whose cloud image reads its first-boot
// config from a NoCloud "cidata" disc, with cloud-config (cloud-init) or an
// Ignition config (Fedora CoreOS).
//
//	create            the machine, born with its identity, its disk a copy of the image
//	guest-init        its own seed disc, with the config rendered from the manifest
//	start-first-boot  started; no console input is ever sent
//	first-boot        watched until the config's last step powers it off, within this apply's deadline
//	boot              started again
//	confirm           accepted once running, as declared, after that power-off
//
// The power-off is the guest's own sign that its config was applied: cloud-init
// powers off at the end of its final stage, and the Ignition config carries a
// unit that powers off once at the end of the first boot. A machine whose
// config failed (Ignition stops in an emergency shell) never powers off and is
// not accepted. Fedora CoreOS ships no guest agent, so nothing else is read
// from inside the machine.

// firstBootPoll is how often a first boot's power state is read. A variable
// so a test can shorten it.
var firstBootPoll = 5 * time.Second

// ImageRecord is the provenance of a build from a disk image: the image as
// the provider listed it, the seed's format and hash, and an image hash only
// as declared. accrete cannot hash a file on the provider's storage.
type ImageRecord struct {
	ImageVolume  string     `json:"image_volume"`
	ImageSize    int64      `json:"image_size"`
	ImageCreated *time.Time `json:"image_created,omitempty"`
	// ImageSHA256Declared is the manifest's image_sha256, never verified:
	// the image was already on the provider's storage.
	ImageSHA256Declared string `json:"image_sha256_declared,omitempty"`
	// ImageURL is where the manifest says the image comes from, and
	// ImageSHA256Fetched the hash of the file this build downloaded from
	// it, checked against the manifest's before it was uploaded.
	ImageURL           string `json:"image_url,omitempty"`
	ImageSHA256Fetched string `json:"image_sha256_fetched,omitempty"`
	SeedFormat         string `json:"seed_format"`
	SeedSHA256         string `json:"seed_sha256,omitempty"`
}

// The steps of a build from a disk image that its others do not share.
const (
	stepStartFirstBoot = "start-first-boot"
	stepFirstBoot      = "first-boot"
)

// imageSeed is the user-data of a machine built from a disk image, in the
// recipe's format: its hostname and the resource's bootstrap profile, its
// strings rewritten by fill. Either way the first boot ends with a
// power-off, after which a seed that carried a secret is replaced.
func imageSeed(rec provider.Recipe, fill filler) (string, error) {
	profile, err := profileOf(rec, fill)
	if err != nil {
		return "", err
	}
	switch rec.Image.SeedFormat {
	case provider.SeedIgnition:
		return bootstrap.RenderIgnition(bootstrap.Base{Hostname: rec.Hostname}, profile)
	case provider.SeedCloudInit:
		return bootstrap.Render(bootstrap.Base{Hostname: rec.Hostname, PowerOffWhenDone: true}, profile)
	}
	return "", fmt.Errorf("seed format %q is not one accrete writes", rec.Image.SeedFormat)
}

func (a *applier) fromImage(run uuid.UUID, rec provider.Recipe) error {
	in, ok := a.writer.(provider.Installer)
	if !ok {
		return errors.New("the provider cannot build machines from disk images")
	}
	started := time.Now()
	for {
		r := a.applying(run)
		if r == nil {
			return nil
		}
		switch r.Step {
		case stepCreate:
			req := rec.Image.Create
			media, checked, err := a.ensureImage(rec)
			if err != nil {
				return fmt.Errorf("image: %w", err)
			}
			r.Image = &ImageRecord{ImageVolume: media.Volume, ImageSize: media.Size, SeedFormat: rec.Image.SeedFormat, ImageURL: rec.Image.URL}
			if checked {
				r.Image.ImageSHA256Fetched = rec.Image.ImageSHA256
			} else {
				r.Image.ImageSHA256Declared = rec.Image.ImageSHA256
			}
			if !media.Created.IsZero() {
				at := media.Created
				r.Image.ImageCreated = &at
			}
			req.Name, req.MintUUID, req.OwnerMark, req.ID = r.Name, r.MintedUUID, r.OwnerMark, ""
			m, err := in.Create(a.ctx, req)
			if err != nil {
				return fmt.Errorf("create: %w", err)
			}
			r.ResourceRefID, r.Step = m.Ref.ID, stepGuestInit
			if m.Found {
				a.printf("  found its earlier machine %s by its owner mark, SMBIOS UUID %s; not created again\n", m.Ref.ID, r.MintedUUID)
			} else {
				a.printf("  created as %s, SMBIOS UUID %s, its disk a copy of %s\n", m.Ref.ID, r.MintedUUID, media.Volume)
			}
		case stepGuestInit:
			ud, err := imageSeed(rec, a.fillFor(true))
			if err != nil {
				return fmt.Errorf("seed: %w", err)
			}
			shown, err := imageSeed(rec, withoutSecrets)
			if err != nil {
				return fmt.Errorf("seed: %w", err)
			}
			pl, err := a.writer.GuestInit(a.ctx, a.target(r), provider.Seed{
				UserData:   []byte(ud),
				MetaData:   []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", r.MintedUUID, rec.Hostname)),
				Storage:    rec.SeedStorage,
				InstanceID: r.MintedUUID,
			})
			if err != nil {
				return fmt.Errorf("seed: %w", err)
			}
			if r.Image == nil {
				r.Image = &ImageRecord{SeedFormat: rec.Image.SeedFormat}
			}
			r.Image.SeedSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(shown)))
			r.Step = stepStartFirstBoot
			a.printf("  seeded with %s (%s, SHA-256 %s)\n", pl.Volume, rec.Image.SeedFormat, r.Image.SeedSHA256)
		case stepStartFirstBoot:
			if _, err := a.writer.Power(a.ctx, a.target(r), provider.PowerRunning); err != nil {
				return fmt.Errorf("start: %w", err)
			}
			r.Step = stepFirstBoot
			a.printf("  started for its first boot; no console input is sent\n")
		case stepFirstBoot:
			deadline := installDeadline(rec.Image.Timeout)
			done, err := a.awaitStopped(r, started.Add(deadline))
			if err != nil {
				return fmt.Errorf("first boot: %w", err)
			}
			if !done {
				return fmt.Errorf("first boot: %s has not powered itself off after the %s this apply waits (first_boot_timeout_minutes), "+
					"so its %s config is not known to have been applied; it is kept as it is: look at its console, run accrete apply again "+
					"to keep waiting, or accrete destroy to remove it", r.ResourceRefID, roundDuration(deadline), rec.Image.SeedFormat)
			}
			r.Step = stepBoot
			if hasSecrets(rec) {
				r.Step = stepScrubSeed
			}
			a.printf("  its first boot applied its %s config and powered it off (this apply waited %s)\n", rec.Image.SeedFormat, roundDuration(time.Since(started)))
		case stepScrubSeed:
			ud, err := imageSeed(rec, withoutSecrets)
			if err != nil {
				return fmt.Errorf("seed: %w", err)
			}
			if err := a.scrubSeed(r, rec, ud); err != nil {
				return err
			}
			r.Step = stepBoot
		case stepBoot:
			if _, err := a.writer.Power(a.ctx, a.target(r), provider.PowerRunning); err != nil {
				return fmt.Errorf("start from its disk: %w", err)
			}
			r.Step = stepConfirm
			a.printf("  started again\n")
		case stepConfirm:
			return a.confirmImage(run, rec)
		default:
			return fmt.Errorf("the state names step %q, which this accrete does not know for a build from a disk image", r.Step)
		}
		if err := a.save(); err != nil {
			return err
		}
	}
}

// awaitStopped watches a machine until it is stopped, or until the deadline.
func (a *applier) awaitStopped(r *Applying, deadline time.Time) (bool, error) {
	for {
		found, err := a.find(r)
		if err != nil {
			return false, err
		}
		if found == nil {
			return false, fmt.Errorf("the provider does not list %s", r.ResourceRefID)
		}
		if found.State == "stopped" {
			return true, nil
		}
		if !time.Now().Add(firstBootPoll).Before(deadline) {
			return false, nil
		}
		select {
		case <-a.ctx.Done():
			return false, a.ctx.Err()
		case <-time.After(firstBootPoll):
		}
	}
}

// confirmImage accepts a machine built from a disk image: it carries its
// minted identity, runs again after its first boot powered it off, and has
// the declared size. A guest agent, when the image has one, must report the
// declared hostname.
func (a *applier) confirmImage(run uuid.UUID, rec provider.Recipe) error {
	r := a.applying(run)
	found, err := a.find(r)
	if err != nil {
		return fmt.Errorf("confirm: %w", err)
	}
	if found == nil {
		return fmt.Errorf("confirm: the provider does not list %s", r.ResourceRefID)
	}
	var wrong []string
	if found.State != "running" {
		wrong = append(wrong, "it is "+found.State+", not running")
	}
	shape := rec.Image.Create.Shape
	if want := shape.Cores; want > 0 && int(found.Facts.CPUCores) != want {
		wrong = append(wrong, fmt.Sprintf("it has %d cores, not %d", found.Facts.CPUCores, want))
	}
	if want := shape.MemoryMB; want > 0 && found.Facts.RAMBytes != int64(want)<<20 {
		wrong = append(wrong, fmt.Sprintf("it has %d MiB of memory, not %d", found.Facts.RAMBytes>>20, want))
	}
	if h := found.Identity.Hostname; h != "" && !strings.EqualFold(h, rec.Hostname) {
		wrong = append(wrong, fmt.Sprintf("its guest agent reports hostname %q, not %q", h, rec.Hostname))
	}
	image := ""
	if r.Image != nil {
		image = fmt.Sprintf("%s, %d bytes", r.Image.ImageVolume, r.Image.ImageSize)
		if r.Image.ImageCreated != nil {
			image += ", dated " + r.Image.ImageCreated.Format(time.RFC3339)
		}
		if r.Image.ImageSHA256Declared != "" {
			image += ", SHA-256 " + r.Image.ImageSHA256Declared + " as declared (not checked)"
		}
		if r.Image.ImageSHA256Fetched != "" {
			image += ", fetched from " + r.Image.ImageURL + " with SHA-256 " + r.Image.ImageSHA256Fetched + " (checked on download)"
		}
		a.printf("  built from %s\n", image)
	}
	return a.record(run, wrong, "confirmed and recorded: its first boot powered it off, and it runs again as declared")
}
