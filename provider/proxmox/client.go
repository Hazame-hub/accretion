// Package proxmox is Proxmox VE for Accretion: the HTTP layer every call to
// the cluster goes through, the guards that keep a call on the guest it was
// meant for, and the discovery Session.
//
// It reads, and it checks. Nothing here changes a machine: the write verbs
// are a later contract (provider.Writer), and until it exists an embedder's
// own code sends the mutations, through Client.Call, with these guards in
// front of them.
package proxmox

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is one connection to a Proxmox VE API. Its fields are exported so
// an embedder that keeps its own (a test server's client, a poll interval
// shortened for tests) can build one without going through NewClient.
type Client struct {
	// BaseURL is the API's origin, e.g. "https://192.168.1.200:8006", with no
	// trailing slash.
	BaseURL string
	// Token is the Authorization header value: "PVEAPIToken=user@realm!id=secret".
	Token string
	// HTTP sends the requests. It should have no client-wide Timeout: each
	// request carries its own deadline (CallTimeout, or an upload's budget).
	HTTP *http.Client
	// CallTimeout bounds one plain API call; zero means DefaultCallTimeout.
	CallTimeout time.Duration
	// TaskPoll is how often a task or a guest's state is re-read.
	TaskPoll time.Duration
}

// DefaultCallTimeout bounds one plain API request: a GET or a form-encoded
// mutation that answers with data or a task reference. Uploads are not plain
// calls and carry their own deadline; long work is a task, waited for by
// TaskWait, whose every poll is again a plain call.
const DefaultCallTimeout = 15 * time.Second

// NewClient validates a connection and builds its client. It does not dial.
//
// verifyTLS false skips certificate verification, which is what a node still
// on its self-signed certificate needs; the caller decides, and says so to
// whoever is watching.
func NewClient(baseURL, apiToken string, verifyTLS bool) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("proxmox: base_url is required")
	}
	if apiToken == "" {
		return nil, fmt.Errorf("proxmox: api_token is required")
	}
	// No client-wide Timeout: it would cap an upload as tightly as a GET.
	// Each request carries its own deadline instead (CallTimeout, or the
	// upload's own budget in StorageUpload).
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: !verifyTLS}, //nolint:gosec
		},
	}
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Token:    NormaliseToken(apiToken),
		HTTP:     httpClient,
		TaskPoll: 2 * time.Second,
	}, nil
}

// NormaliseToken returns the Authorization header value for a token.
// Proxmox requires the "PVEAPIToken=" prefix; the raw token
// (user@realm!tokenid=secret) is accepted and the prefix prepended.
func NormaliseToken(apiToken string) string {
	if !strings.HasPrefix(apiToken, "PVEAPIToken=") {
		return "PVEAPIToken=" + apiToken
	}
	return apiToken
}

func (c *Client) callTimeout() time.Duration {
	if c.CallTimeout > 0 {
		return c.CallTimeout
	}
	return DefaultCallTimeout
}

// APIError is an HTTP-level refusal from the Proxmox API, kept as a type so
// idempotency rechecks can distinguish "the API said no" from "the network
// died": only the former is worth a does-it-already-exist look.
type APIError struct {
	Code int
	Body string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("proxmox: API error %d: %s", e.Code, e.Body)
}

// Call performs one API request. A non-nil form is sent url-encoded, which is
// what the Proxmox API expects for every mutation. The returned bytes are the
// contents of the response's "data" envelope.
func (c *Client) Call(ctx context.Context, method, path string, form url.Values) (json.RawMessage, error) {
	data, _, err := c.CallEnvelope(ctx, method, path, form)
	return data, err
}

// CallEnvelope is Call that also returns the envelope's "changes" attribute,
// which Proxmox sets beside "data": on GET /nodes/<n>/network it is the diff
// of network edits staged but not applied.
func (c *Client) CallEnvelope(ctx context.Context, method, path string, form url.Values) (json.RawMessage, string, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api2/json"+path, body)
	if err != nil {
		return nil, "", fmt.Errorf("proxmox: build request: %w", err)
	}
	req.Header.Set("Authorization", c.Token)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("proxmox: request %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return nil, "", &APIError{Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	var wrapper struct {
		Data    json.RawMessage `json:"data"`
		Changes string          `json:"changes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return nil, "", fmt.Errorf("proxmox: decode response: %w", err)
	}
	return wrapper.Data, wrapper.Changes, nil
}

// Get reads one path into dest; a nil dest discards the answer.
func (c *Client) Get(ctx context.Context, path string, dest interface{}) error {
	data, err := c.Call(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if dest != nil {
		return json.Unmarshal(data, dest)
	}
	return nil
}

// StorageUpload posts a file to a node's storage as one content type and waits
// for the task that stores it. The API takes iso, vztmpl and import here and
// nothing else: a snippets upload is refused by Proxmox VE 9 by name.
//
// timeout bounds the transfer, and then, separately, the wait for the task
// that stores the file: an upload to slow storage is not held to the 15
// seconds a plain call gets.
func (c *Client) StorageUpload(ctx context.Context, node, storage, content, filename string, body []byte, timeout time.Duration) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("content", content); err != nil {
		return err
	}
	fw, err := w.CreateFormFile("filename", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	url := fmt.Sprintf("%s/api2/json/nodes/%s/storage/%s/upload", c.BaseURL, node, storage)
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.Token)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("proxmox: upload %s: %w", filename, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return &APIError{Code: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	var wrapper struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wrapper); err != nil {
		return fmt.Errorf("proxmox: decode upload response: %w", err)
	}
	return c.WaitIfTask(ctx, node, wrapper.Data, timeout)
}
