// Package redfish drives a physical server through its BMC (iDRAC, iLO, or
// any DMTF Redfish service): it reads the server's identity, power state,
// boot override and virtual media, and can insert and eject media, set a
// one-time boot device and reset power. That is what an unattended install
// needs, and no more: no BIOS settings, RAID, firmware or vendor OEM actions.
//
// Every write first reads the system and refuses, sending nothing, unless the
// BMC reports the serial number or UUID the operator declared (Guard). A typo
// in a BMC address therefore cannot reinstall another server.
//
// Verbs keep the rules of provider.Writer: idempotent, with three kinds of
// failure (*provider.RefusedError when nothing was sent, a plain error when
// the outcome is known, *provider.OutcomeUnknownError when the answer was
// lost). Values the BMC chooses from (boot targets, reset types, media types)
// are taken from what it lists as allowed, never assumed.
//
// Design: sybil-dev openspec/changes/accrete-bare-metal-install, D5 to D7.
package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hazame-hub/accretion/provider"
)

// Config is how to reach one BMC.
type Config struct {
	// Endpoint is the BMC's base URL, https://bmc.example (the service root
	// /redfish/v1 is appended).
	Endpoint string
	Username string
	Password string
	// InsecureSkipVerify turns TLS verification off. Many BMCs ship a
	// self-signed certificate; turning verification off must be this
	// explicit, and Insecure reports it so a command can say so.
	InsecureSkipVerify bool
	// AllowHTTP accepts an http:// endpoint, for an emulator on localhost.
	AllowHTTP bool
	// Timeout bounds one request. 0 means 30 s.
	Timeout time.Duration
	// HTTPClient replaces the client built from the fields above (tests).
	HTTPClient *http.Client
}

// String hides the credential.
func (c Config) String() string { return "redfish.Config{" + c.Endpoint + "}" }

// Client talks to one BMC.
type Client struct {
	base     *url.URL
	user     string
	pass     string
	http     *http.Client
	insecure bool
}

// String hides the credential.
func (c *Client) String() string { return "redfish.Client{" + c.base.String() + "}" }

// Insecure reports whether TLS is not verified, or not used.
func (c *Client) Insecure() bool { return c.insecure }

// New checks cfg and returns a client. Nothing is sent.
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(cfg.Endpoint, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("redfish: endpoint %q is not a URL", cfg.Endpoint)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !cfg.AllowHTTP {
			return nil, fmt.Errorf("redfish: endpoint %q is plain http; a BMC credential would travel in clear (allow it explicitly only for an emulator)", cfg.Endpoint)
		}
	default:
		return nil, fmt.Errorf("redfish: endpoint %q: scheme must be https", cfg.Endpoint)
	}
	if u.User != nil {
		return nil, fmt.Errorf("redfish: put the credential in Username and Password, not in the endpoint URL")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if cfg.InsecureSkipVerify {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit operator choice, reported by Insecure
		}
		hc = &http.Client{Transport: tr, Timeout: timeout}
	}
	return &Client{base: u, user: cfg.Username, pass: cfg.Password, http: hc, insecure: cfg.InsecureSkipVerify || u.Scheme == "http"}, nil
}

// System is what the BMC reports about one server.
type System struct {
	// Path is the system's @odata.id, what every verb takes.
	Path         string
	ID           string
	UUID         string
	Serial       string
	Manufacturer string
	Model        string
	// Power is the BMC's PowerState: On, Off, PoweringOn, PoweringOff.
	Power string
	Boot  Boot
	// ResetTypes are the reset types the BMC allows.
	ResetTypes []string
	resetPath  string
	// Media are the virtual media slots, under the system or under the
	// manager that manages it (older iDRAC and iLO firmware).
	Media []MediaSlot
}

// Boot is the system's boot override.
type Boot struct {
	// Enabled is Disabled, Once or Continuous.
	Enabled string
	Target  string
	// AllowedTargets are the targets the BMC lists as allowed.
	AllowedTargets []string
}

// MediaSlot is one virtual media device.
type MediaSlot struct {
	Path       string
	ID         string
	MediaTypes []string
	Inserted   bool
	Image      string
	insertPath string
	ejectPath  string
}

// Accepts reports whether the slot lists one of the media types.
func (m MediaSlot) Accepts(types ...string) bool {
	for _, have := range m.MediaTypes {
		for _, t := range types {
			if strings.EqualFold(have, t) {
				return true
			}
		}
	}
	return false
}

// Systems lists the paths of the systems the BMC manages. A BMC usually
// manages one; a chassis manager may list several.
func (c *Client) Systems(ctx context.Context) ([]string, error) {
	var coll collection
	if err := c.get(ctx, "/redfish/v1/Systems", &coll); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(coll.Members))
	for _, m := range coll.Members {
		out = append(out, m.ID)
	}
	return out, nil
}

// System reads one system, its reset types and its virtual media.
func (c *Client) System(ctx context.Context, path string) (System, error) {
	var raw rawSystem
	if err := c.get(ctx, path, &raw); err != nil {
		return System{}, err
	}
	s := System{
		Path: raw.OdataID, ID: raw.ID, UUID: strings.ToLower(strings.TrimSpace(raw.UUID)),
		Serial: strings.TrimSpace(raw.SerialNumber), Manufacturer: raw.Manufacturer, Model: raw.Model,
		Power: raw.PowerState,
		Boot: Boot{
			Enabled: raw.Boot.BootSourceOverrideEnabled, Target: raw.Boot.BootSourceOverrideTarget,
			AllowedTargets: raw.Boot.AllowedTargets,
		},
		ResetTypes: raw.Actions.Reset.AllowableValues,
		resetPath:  raw.Actions.Reset.Target,
	}
	if s.Path == "" {
		s.Path = path
	}
	mediaColl := raw.VirtualMedia.ID
	if mediaColl == "" && len(raw.Links.ManagedBy) > 0 {
		var mgr rawManager
		if err := c.get(ctx, raw.Links.ManagedBy[0].ID, &mgr); err != nil {
			return System{}, err
		}
		mediaColl = mgr.VirtualMedia.ID
	}
	if mediaColl != "" {
		var coll collection
		if err := c.get(ctx, mediaColl, &coll); err != nil {
			return System{}, err
		}
		for _, m := range coll.Members {
			var rm rawMedia
			if err := c.get(ctx, m.ID, &rm); err != nil {
				return System{}, err
			}
			slot := MediaSlot{
				Path: rm.OdataID, ID: rm.ID, MediaTypes: rm.MediaTypes, Inserted: rm.Inserted, Image: rm.Image,
				insertPath: rm.Actions.Insert.Target, ejectPath: rm.Actions.Eject.Target,
			}
			if slot.Path == "" {
				slot.Path = m.ID
			}
			s.Media = append(s.Media, slot)
		}
	}
	return s, nil
}

// Guard is the identity the operator declared for the server. At least one
// field is required; when both are set, both must match.
type Guard struct {
	Serial string
	UUID   string
}

// placeholderSerials are what boards ship instead of a serial number; a guard
// on one of them guards nothing. Kept in step with package bundle.
var placeholderSerials = map[string]bool{
	"to be filled by o.e.m.": true, "default string": true, "system serial number": true,
	"0123456789": true, "not specified": true, "none": true, "n/a": true, "0": true,
	"00000000": true, "chassis serial number": true, "not applicable": true,
}

// Check compares the guard with what the BMC reported. It is what every verb
// calls before sending anything; it is exported so plan can show the answer.
func (g Guard) Check(s System) error {
	serial, uuid := strings.TrimSpace(g.Serial), strings.TrimSpace(g.UUID)
	if serial == "" && uuid == "" {
		return provider.Refused("redfish: no serial number or UUID declared for %s; nothing guards against a BMC address that points at another server", s.Path)
	}
	if serial != "" {
		if placeholderSerials[strings.ToLower(serial)] {
			return provider.Refused("redfish: declared serial %q is a placeholder boards ship, not an identity", serial)
		}
		if !strings.EqualFold(serial, s.Serial) {
			return provider.Refused("redfish: the BMC reports serial %q, but %q was declared; this BMC may belong to another server, so nothing was sent", s.Serial, serial)
		}
	}
	if uuid != "" {
		want := provider.UUIDForms(uuid)
		if want == nil {
			return provider.Refused("redfish: declared UUID %q is malformed or a placeholder", uuid)
		}
		match := false
		for _, f := range want {
			if f == s.UUID {
				match = true
			}
		}
		if !match {
			return provider.Refused("redfish: the BMC reports UUID %q, but %q was declared; this BMC may belong to another server, so nothing was sent", s.UUID, uuid)
		}
	}
	return nil
}

// Result says whether a verb changed anything.
type Result struct {
	// Applied is false when the system was already in the asked state.
	Applied bool
}

// guarded reads the system and checks the guard.
func (c *Client) guarded(ctx context.Context, path string, g Guard) (System, error) {
	s, err := c.System(ctx, path)
	if err != nil {
		return System{}, err
	}
	if err := g.Check(s); err != nil {
		return System{}, err
	}
	return s, nil
}

func slotByID(s System, id string) (MediaSlot, error) {
	for _, m := range s.Media {
		if m.ID == id {
			return m, nil
		}
	}
	ids := make([]string, len(s.Media))
	for i, m := range s.Media {
		ids[i] = m.ID
	}
	return MediaSlot{}, provider.Refused("redfish: %s has no virtual media slot %q (it has %s)", s.Path, id, listOrNone(ids))
}

// InsertMedia mounts image (a URL the BMC fetches) in the slot, read-only.
// It refuses a slot that holds another image: that media is not accrete's to
// eject.
func (c *Client) InsertMedia(ctx context.Context, path string, g Guard, slotID, image string) (Result, error) {
	if u, err := url.Parse(image); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Result{}, provider.Refused("redfish: media image %q must be an http(s) URL the BMC can reach", image)
	}
	s, err := c.guarded(ctx, path, g)
	if err != nil {
		return Result{}, err
	}
	slot, err := slotByID(s, slotID)
	if err != nil {
		return Result{}, err
	}
	if slot.Inserted {
		if slot.Image == image {
			return Result{Applied: false}, nil
		}
		return Result{}, provider.Refused("redfish: slot %q already holds %q; eject it first if it is not in use", slotID, slot.Image)
	}
	target := slot.insertPath
	if target == "" {
		target = slot.Path + "/Actions/VirtualMedia.InsertMedia"
	}
	body := map[string]any{"Image": image, "Inserted": true, "WriteProtected": true}
	if err := c.post(ctx, "insert media", target, body); err != nil {
		return Result{}, err
	}
	return Result{Applied: true}, nil
}

// EjectMedia ejects the slot. image is what accrete inserted there: a slot
// holding another image is refused, so accrete never ejects media it did not
// put in. An empty slot succeeds with nothing done.
func (c *Client) EjectMedia(ctx context.Context, path string, g Guard, slotID, image string) (Result, error) {
	s, err := c.guarded(ctx, path, g)
	if err != nil {
		return Result{}, err
	}
	slot, err := slotByID(s, slotID)
	if err != nil {
		return Result{}, err
	}
	if !slot.Inserted {
		return Result{Applied: false}, nil
	}
	if slot.Image != image {
		return Result{}, provider.Refused("redfish: slot %q holds %q, not %q; it was not inserted by this machine's install, so it is left in place", slotID, slot.Image, image)
	}
	target := slot.ejectPath
	if target == "" {
		target = slot.Path + "/Actions/VirtualMedia.EjectMedia"
	}
	if err := c.post(ctx, "eject media", target, map[string]any{}); err != nil {
		return Result{}, err
	}
	return Result{Applied: true}, nil
}

// BootOnce sets the next boot, and only the next, to the first of targets
// the BMC allows (for an installer CD: "Cd", then "UsbCd" on firmware that
// names it so).
func (c *Client) BootOnce(ctx context.Context, path string, g Guard, targets ...string) (Result, error) {
	s, err := c.guarded(ctx, path, g)
	if err != nil {
		return Result{}, err
	}
	target := pick(s.Boot.AllowedTargets, targets)
	if target == "" {
		return Result{}, provider.Refused("redfish: %s allows boot targets %s, none of %s", s.Path, listOrNone(s.Boot.AllowedTargets), strings.Join(targets, ", "))
	}
	if s.Boot.Enabled == "Once" && s.Boot.Target == target {
		return Result{Applied: false}, nil
	}
	body := map[string]any{"Boot": map[string]any{"BootSourceOverrideEnabled": "Once", "BootSourceOverrideTarget": target}}
	if err := c.send(ctx, http.MethodPatch, "set boot override", s.Path, body); err != nil {
		return Result{}, err
	}
	return Result{Applied: true}, nil
}

// PowerOn powers the system on. A system already on succeeds with nothing
// done.
func (c *Client) PowerOn(ctx context.Context, path string, g Guard) (Result, error) {
	s, err := c.guarded(ctx, path, g)
	if err != nil {
		return Result{}, err
	}
	if s.Power == "On" {
		return Result{Applied: false}, nil
	}
	return c.reset(ctx, s, "power on", "On", "ForceOn")
}

// PowerOff asks the operating system to shut down, or with force cuts power.
// A system already off succeeds with nothing done.
func (c *Client) PowerOff(ctx context.Context, path string, g Guard, force bool) (Result, error) {
	s, err := c.guarded(ctx, path, g)
	if err != nil {
		return Result{}, err
	}
	if s.Power == "Off" {
		return Result{Applied: false}, nil
	}
	if force {
		return c.reset(ctx, s, "force power off", "ForceOff")
	}
	return c.reset(ctx, s, "shut down", "GracefulShutdown")
}

func (c *Client) reset(ctx context.Context, s System, op string, types ...string) (Result, error) {
	t := pick(s.ResetTypes, types)
	if t == "" {
		return Result{}, provider.Refused("redfish: %s allows reset types %s, none of %s; nothing was sent", s.Path, listOrNone(s.ResetTypes), strings.Join(types, ", "))
	}
	target := s.resetPath
	if target == "" {
		target = s.Path + "/Actions/ComputerSystem.Reset"
	}
	if err := c.post(ctx, op, target, map[string]any{"ResetType": t}); err != nil {
		return Result{}, err
	}
	return Result{Applied: true}, nil
}

// pick returns the first of want that allowed lists, compared case-blind,
// spelled as the BMC spells it.
func pick(allowed, want []string) string {
	for _, w := range want {
		for _, a := range allowed {
			if strings.EqualFold(a, w) {
				return a
			}
		}
	}
	return ""
}

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// --- transport

// HTTPError is a request the BMC answered with an error status. The request
// reached the BMC and was refused, so its outcome is known: nothing changed.
type HTTPError struct {
	Op     string
	Status int
	// Message is the BMC's own message, when it gave one.
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("redfish: %s: HTTP %d: %s", e.Op, e.Status, e.Message)
	}
	return fmt.Sprintf("redfish: %s: HTTP %d", e.Op, e.Status)
}

// Is makes a 401 match provider.ErrUnauthorized.
func (e *HTTPError) Is(target error) bool {
	return target == provider.ErrUnauthorized && e.Status == http.StatusUnauthorized
}

func (c *Client) resolve(path string) (string, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("redfish: bad path %q", path)
	}
	if ref.IsAbs() || ref.Host != "" {
		// A BMC answers with paths, not URLs; one that sends a URL to
		// another host does not get the credential sent there.
		return "", fmt.Errorf("redfish: the BMC pointed at %q, outside %s; not followed", path, c.base.Host)
	}
	return c.base.ResolveReference(ref).String(), nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	u, err := c.resolve(path)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		// A read that failed changed nothing.
		return fmt.Errorf("redfish: read %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("redfish: read %s: %w", path, err)
	}
	if resp.StatusCode >= 300 {
		return &HTTPError{Op: "read " + path, Status: resp.StatusCode, Message: message(data)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("redfish: read %s: not Redfish JSON: %w", path, err)
	}
	return nil
}

func (c *Client) post(ctx context.Context, op, path string, body any) error {
	return c.send(ctx, http.MethodPost, op, path, body)
}

// send makes a change. A transport failure after the request may have gone
// out is *provider.OutcomeUnknownError: the BMC may have acted.
func (c *Client) send(ctx context.Context, method, op, path string, body any) error {
	u, err := c.resolve(path)
	if err != nil {
		return provider.Refused("%v", err)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return &provider.OutcomeUnknownError{Op: "redfish: " + op, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return &HTTPError{Op: op, Status: resp.StatusCode, Message: message(rb)}
	}
	return nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OData-Version", "4.0")
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			// url.Error repeats the URL, which holds no credential (New
			// refuses userinfo); keep only the cause.
			return nil, ue.Err
		}
		return nil, err
	}
	return resp, nil
}

// message extracts the BMC's error message, short.
func message(data []byte) string {
	var e struct {
		Error struct {
			Message  string `json:"message"`
			Extended []struct {
				Message string `json:"Message"`
			} `json:"@Message.ExtendedInfo"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return ""
	}
	m := e.Error.Message
	if len(e.Error.Extended) > 0 && e.Error.Extended[0].Message != "" {
		m = e.Error.Extended[0].Message
	}
	if len(m) > 300 {
		m = m[:300] + "..."
	}
	return m
}

// --- wire types

type odataRef struct {
	ID string `json:"@odata.id"`
}

type collection struct {
	Members []odataRef `json:"Members"`
}

type rawSystem struct {
	OdataID      string `json:"@odata.id"`
	ID           string `json:"Id"`
	UUID         string `json:"UUID"`
	SerialNumber string `json:"SerialNumber"`
	Manufacturer string `json:"Manufacturer"`
	Model        string `json:"Model"`
	PowerState   string `json:"PowerState"`
	Boot         struct {
		BootSourceOverrideEnabled string   `json:"BootSourceOverrideEnabled"`
		BootSourceOverrideTarget  string   `json:"BootSourceOverrideTarget"`
		AllowedTargets            []string `json:"BootSourceOverrideTarget@Redfish.AllowableValues"`
	} `json:"Boot"`
	VirtualMedia odataRef `json:"VirtualMedia"`
	Links        struct {
		ManagedBy []odataRef `json:"ManagedBy"`
	} `json:"Links"`
	Actions struct {
		Reset struct {
			Target          string   `json:"target"`
			AllowableValues []string `json:"ResetType@Redfish.AllowableValues"`
		} `json:"#ComputerSystem.Reset"`
	} `json:"Actions"`
}

type rawManager struct {
	VirtualMedia odataRef `json:"VirtualMedia"`
}

type rawMedia struct {
	OdataID    string   `json:"@odata.id"`
	ID         string   `json:"Id"`
	MediaTypes []string `json:"MediaTypes"`
	Inserted   bool     `json:"Inserted"`
	Image      string   `json:"Image"`
	Actions    struct {
		Insert struct {
			Target string `json:"target"`
		} `json:"#VirtualMedia.InsertMedia"`
		Eject struct {
			Target string `json:"target"`
		} `json:"#VirtualMedia.EjectMedia"`
	} `json:"Actions"`
}
