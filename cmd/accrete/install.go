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

// An install from installer media (openspec accrete-installer-iso): the
// steps of D6 and the acceptance of D7.
//
//	create           an empty machine with its identity, its disk and the installer
//	answer           its own answer disc, with the kickstart rendered from the manifest
//	start-installer  started; no console input is ever sent
//	installing       watched until the installer powers it off, within this apply's deadline
//	remove-media     both discs ejected, the answer disc deleted, the installer kept
//	boot             started from its own disk
//	accept           accepted only once its guest agent reports the declared hostname
//
// Each step is saved before the next starts. The installer carries on when
// accrete dies, so a rerun resumes watching the same machine.

// installPoll is how often an install's power state is read, and agentWait
// how long the guest agent of a machine started from its new disk has to
// report its hostname. Variables so a test can shorten them.
var (
	installPoll = 15 * time.Second
	agentWait   = 5 * time.Minute
	agentPoll   = 5 * time.Second
	// installDeadline is the deadline an apply waits for an install, from
	// the manifest's.
	installDeadline = func(d time.Duration) time.Duration { return d }
)

// firstStep is where a new machine's build starts.
func firstStep(rec provider.Recipe) string {
	if rec.Install != nil {
		return stepCreate
	}
	return stepClone
}

// kickstart is the answer file of a machine installed from installer media:
// its hostname and the resource's bootstrap profile, which may reference no
// secret (the disc is readable from storage).
func kickstart(rec provider.Recipe) (string, error) {
	var profile *bootstrap.Profile
	if rec.Bootstrap != nil {
		p, err := bootstrap.Parse(rec.Bootstrap)
		if err != nil {
			return "", err
		}
		if err := p.Strings(func(s string) (string, error) {
			if strings.Contains(s, "{{secret.") {
				return "", errors.New("references a secret; an answer disc is readable from storage, so an install takes no secret")
			}
			return s, nil
		}); err != nil {
			return "", err
		}
		profile = p
	}
	return bootstrap.RenderKickstart(bootstrap.Install{Hostname: rec.Hostname, Disk: rec.Install.Disk}, profile)
}

func (a *applier) install(run uuid.UUID, rec provider.Recipe) error {
	in, ok := a.writer.(provider.Installer)
	if !ok {
		return errors.New("the provider cannot install machines from installer media")
	}
	started := time.Now()
	for {
		r := a.applying(run)
		if r == nil {
			return nil
		}
		switch r.Step {
		case stepCreate:
			req := rec.Install.Create
			media, err := in.Media(a.ctx, req.Placement, req.Installer)
			if err != nil {
				return fmt.Errorf("installer: %w", err)
			}
			r.Install = &InstallRecord{ISOVolume: media.Volume, ISOSize: media.Size, ISOSHA256Declared: rec.Install.ISOSHA256, AnswerFormat: rec.Install.AnswerFormat}
			if !media.Created.IsZero() {
				at := media.Created
				r.Install.ISOCreated = &at
			}
			req.Name, req.MintUUID, req.OwnerMark, req.ID = r.Name, r.MintedUUID, r.OwnerMark, ""
			m, err := in.Create(a.ctx, req)
			if err != nil {
				return fmt.Errorf("create: %w", err)
			}
			r.ResourceRefID, r.Step = m.Ref.ID, stepAnswer
			if m.Found {
				a.printf("  found its earlier machine %s by its owner mark, SMBIOS UUID %s; not created again\n", m.Ref.ID, r.MintedUUID)
			} else {
				a.printf("  created as %s, SMBIOS UUID %s, empty %d GiB disk, booting %s\n", m.Ref.ID, r.MintedUUID, req.Shape.DiskGB, media.Volume)
			}
		case stepAnswer:
			ks, err := kickstart(rec)
			if err != nil {
				return fmt.Errorf("answer file: %w", err)
			}
			pl, err := in.PlaceAnswer(a.ctx, a.target(r), provider.Answer{
				Label: bootstrap.KickstartLabel, Files: map[string][]byte{bootstrap.KickstartFile: []byte(ks)},
				Storage: rec.SeedStorage, InstanceID: r.MintedUUID,
			})
			if err != nil {
				return fmt.Errorf("answer disc: %w", err)
			}
			r.Install.AnswerSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(ks)))
			r.Step = stepStartInstaller
			a.printf("  answer disc %s on %s (label %s, %s SHA-256 %s)\n", pl.Volume, pl.Drive, bootstrap.KickstartLabel, bootstrap.KickstartFile, r.Install.AnswerSHA256)
		case stepStartInstaller:
			if _, err := a.writer.Power(a.ctx, a.target(r), provider.PowerRunning); err != nil {
				return fmt.Errorf("start the installer: %w", err)
			}
			r.Step = stepInstalling
			a.printf("  started; the installer runs on its own, no console input is sent\n")
		case stepInstalling:
			deadline := installDeadline(rec.Install.Timeout)
			done, err := a.awaitInstall(r, started.Add(deadline))
			if err != nil {
				return fmt.Errorf("install: %w", err)
			}
			if !done {
				return fmt.Errorf("install: %s is still running its installer after the %s this apply waits (install_timeout_minutes); "+
					"it is kept as it is, discs attached: run accrete apply again to keep waiting, or accrete destroy to remove it",
					r.ResourceRefID, roundDuration(deadline))
			}
			r.Step = stepRemoveMedia
			a.printf("  the installer powered it off (this apply waited %s)\n", roundDuration(time.Since(started)))
		case stepRemoveMedia:
			if _, err := in.RemoveMedia(a.ctx, a.target(r), rec.Seed(r.MintedUUID)); err != nil {
				return fmt.Errorf("remove the discs: %w", err)
			}
			r.Step = stepBoot
			a.printf("  discs ejected and its answer disc deleted; the installer ISO stays\n")
		case stepBoot:
			if _, err := a.writer.Power(a.ctx, a.target(r), provider.PowerRunning); err != nil {
				return fmt.Errorf("start from its disk: %w", err)
			}
			r.Step = stepAccept
			a.printf("  started from its own disk\n")
		case stepAccept:
			return a.acceptInstall(run, rec)
		default:
			return fmt.Errorf("the state names install step %q, which this accrete does not know", r.Step)
		}
		if err := a.save(); err != nil {
			return err
		}
	}
}

// roundDuration is d to the second, or as it is below one.
func roundDuration(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}

// find is the provider's listing of a machine being built, checked to carry
// its minted identity. nil when it is not listed.
func (a *applier) find(r *Applying) (*provider.Resource, error) {
	resources, err := a.session.Discover(a.ctx)
	if err != nil && len(resources) == 0 {
		return nil, fmt.Errorf("read the provider: %w", err)
	}
	for i := range resources {
		if resources[i].Ref.ID != r.ResourceRefID {
			continue
		}
		if !sameUUID(resources[i].Identity.SMBIOSUUID, provider.UUIDForms(r.MintedUUID)) {
			return nil, fmt.Errorf("%s carries SMBIOS UUID %q, not the minted %s", r.ResourceRefID, resources[i].Identity.SMBIOSUUID, r.MintedUUID)
		}
		return &resources[i], nil
	}
	return nil, nil
}

// awaitInstall watches an install until the machine is stopped, which the
// kickstart's poweroff does at its end, or until the deadline.
func (a *applier) awaitInstall(r *Applying, deadline time.Time) (bool, error) {
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
		if !time.Now().Add(installPoll).Before(deadline) {
			return false, nil
		}
		select {
		case <-a.ctx.Done():
			return false, a.ctx.Err()
		case <-time.After(installPoll):
		}
	}
}

// acceptInstall is D7: the machine powered off (the installing step), its
// discs were removed before this start (remove-media), and now its guest
// agent answers with the declared hostname on the machine carrying the
// minted identity. Anything short of that leaves the record unknown and the
// machine kept: a rerun checks again.
func (a *applier) acceptInstall(run uuid.UUID, rec provider.Recipe) error {
	r := a.applying(run)
	until := time.Now().Add(agentWait)
	var found *provider.Resource
	for {
		f, err := a.find(r)
		if err != nil {
			return fmt.Errorf("accept: %w", err)
		}
		if f != nil && f.State == "running" && f.Identity.Hostname != "" {
			found = f
			break
		}
		if !time.Now().Add(agentPoll).Before(until) {
			state := "not listed"
			if f != nil {
				state = f.State
			}
			return fmt.Errorf("accept: %s (%s) started from its disk, but its guest agent did not report a hostname within %s; "+
				"it is not accepted, its record stays unknown and the machine is kept: run accrete apply again to check again, or accrete destroy to remove it",
				r.ResourceRefID, state, agentWait)
		}
		select {
		case <-a.ctx.Done():
			return a.ctx.Err()
		case <-time.After(agentPoll):
		}
	}
	if !strings.EqualFold(found.Identity.Hostname, rec.Hostname) {
		return fmt.Errorf("accept: the guest agent of %s reports hostname %q, not the declared %q; the answer file was not applied as written, "+
			"so it is not accepted: its record stays unknown and the machine is kept", r.ResourceRefID, found.Identity.Hostname, rec.Hostname)
	}
	var wrong []string
	if want := rec.Install.Create.Shape.Cores; want > 0 && int(found.Facts.CPUCores) != want {
		wrong = append(wrong, fmt.Sprintf("it has %d cores, not %d", found.Facts.CPUCores, want))
	}
	if want := rec.Install.Create.Shape.MemoryMB; want > 0 && found.Facts.RAMBytes != int64(want)<<20 {
		wrong = append(wrong, fmt.Sprintf("it has %d MiB of memory, not %d", found.Facts.RAMBytes>>20, want))
	}
	r.Install.Hostname = found.Identity.Hostname
	iso := fmt.Sprintf("%s, %d bytes", r.Install.ISOVolume, r.Install.ISOSize)
	if r.Install.ISOCreated != nil {
		iso += ", dated " + r.Install.ISOCreated.Format(time.RFC3339)
	}
	if r.Install.ISOSHA256Declared != "" {
		iso += ", SHA-256 " + r.Install.ISOSHA256Declared + " as declared (not checked)"
	}
	a.printf("  installed from %s\n", iso)
	return a.record(run, wrong, fmt.Sprintf("accepted and recorded: its guest agent reports %s after a start from its own disk", found.Identity.Hostname))
}
