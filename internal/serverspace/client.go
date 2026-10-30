// Package serverspace is a small client for the Serverspace vStack API,
// limited to what a node that lives for one session needs: find a location
// and an image, quote a price, create, wait, look up, delete, read balance.
//
// API reference: https://docs.serverspace.ru/docs/ru/v1/openapi.json
package serverspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the ruble-billed API. The .io one is a separate account.
const DefaultBaseURL = "https://api.serverspace.ru"

// ErrNotFound is returned when the server (or task) does not exist anymore.
var ErrNotFound = errors.New("not found")

type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

func New(key string) *Client {
	return &Client{BaseURL: DefaultBaseURL, Key: key, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type Location struct {
	ID              string `json:"id"`
	SystemVolumeMin int    `json:"system_volume_min"` // MB
	BandwidthMin    int    `json:"bandwidth_min"`
	CPUOptions      []int  `json:"cpu_quantity_options"`
	RAMOptions      []int  `json:"ram_size_options"` // MB
	AllowCreating   bool   `json:"allow_creating_servers"`
}

type Image struct {
	ID           string `json:"id"`
	LocationID   string `json:"location_id"`
	Type         string `json:"type"` // Ubuntu, Debian, ...
	OSVersion    string `json:"os_version"`
	Architecture string `json:"architecture"` // X64
}

// Spec is the server to create or to price.
type Spec struct {
	Name          string
	LocationID    string
	ImageID       string
	CPU           int
	RAMMB         int
	DiskMB        int
	BandwidthMbps int
	Tags          []string
}

type NIC struct {
	IP          string `json:"ip_address"`
	NetworkType string `json:"network_type"` // PublicShared or Isolated
}

type Server struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	LocationID string    `json:"location_id"`
	State      string    `json:"state"` // New, Active, Busy, Blocked
	PowerOn    bool      `json:"is_power_on"`
	NICs       []NIC     `json:"nics"`
	Tags       []string  `json:"tags"`
	Created    time.Time `json:"created"`
	// Login and Password are the root login the hoster set; the app uses
	// them once, to install the node over SSH.
	Login    string `json:"login"`
	Password string `json:"password"`
}

// PublicIP is the address clients connect to, empty until the network is up.
func (s Server) PublicIP() string {
	for _, n := range s.NICs {
		if n.NetworkType == "PublicShared" && n.IP != "" {
			return n.IP
		}
	}
	return ""
}

type Task struct {
	ID       string `json:"id"`
	State    string `json:"is_completed"` // New, InProgress, Completed, Failed, Canceled
	ServerID string `json:"server_id"`
	Progress int    `json:"progress_percent"`
}

func (t Task) Done() bool   { return t.State == "Completed" }
func (t Task) Failed() bool { return t.State == "Failed" || t.State == "Canceled" }

type Balance struct {
	Amount   float64
	Currency string
}

func (c *Client) Locations(ctx context.Context) ([]Location, error) {
	var r struct {
		Locations []Location `json:"locations"`
	}
	return r.Locations, c.do(ctx, http.MethodGet, "/api/v1/locations", nil, &r)
}

func (c *Client) Images(ctx context.Context, location string) ([]Image, error) {
	var r struct {
		Images []Image `json:"images"`
	}
	return r.Images, c.do(ctx, http.MethodGet, "/api/v1/images?location_id="+location, nil, &r)
}

// MonthlyPrice is what the server would cost per month in the account currency.
// Serverspace's hourly price is this divided by 720.
func (c *Client) MonthlyPrice(ctx context.Context, s Spec) (float64, error) {
	body := map[string]any{
		"location_id": s.LocationID,
		"image_id":    s.ImageID,
		"cpu":         s.CPU,
		"ram_mb":      s.RAMMB,
		"volumes":     []any{map[string]any{"size_mb": s.DiskMB}},
		"networks":    []any{map[string]any{"bandwidth_mbps": s.BandwidthMbps}},
	}
	var r struct {
		Price float64 `json:"price"`
	}
	return r.Price, c.do(ctx, http.MethodPost, "/api/v1/servers/price", body, &r)
}

// Create starts server creation and returns the task to wait for.
func (c *Client) Create(ctx context.Context, s Spec) (string, error) {
	// Serverspace's own Go client sends only these fields; the schema marks
	// backup and applications as required but the API does not enforce it.
	body := map[string]any{
		"name":        s.Name,
		"location_id": s.LocationID,
		"image_id":    s.ImageID,
		"cpu":         s.CPU,
		"ram_mb":      s.RAMMB,
		"volumes":     []any{map[string]any{"name": "boot", "size_mb": s.DiskMB}},
		"networks":    []any{map[string]any{"bandwidth_mbps": s.BandwidthMbps}},
		"tags":        s.Tags,
	}
	var r struct {
		TaskID string `json:"task_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/servers", body, &r); err != nil {
		return "", err
	}
	return r.TaskID, nil
}

func (c *Client) Task(ctx context.Context, id string) (Task, error) {
	var r struct {
		Task Task `json:"task"`
	}
	return r.Task, c.do(ctx, http.MethodGet, "/api/v1/tasks/"+id, nil, &r)
}

// WaitTask polls until the task completes, fails or ctx ends.
func (c *Client) WaitTask(ctx context.Context, id string, every time.Duration) (Task, error) {
	for {
		t, err := c.Task(ctx, id)
		switch {
		case err != nil && !errors.Is(err, ErrNotFound):
			return t, err
		case t.Done():
			return t, nil
		case t.Failed():
			return t, fmt.Errorf("task %s: %s", id, t.State)
		}
		select {
		case <-ctx.Done():
			return t, ctx.Err()
		case <-time.After(every):
		}
	}
}

func (c *Client) Server(ctx context.Context, id string) (Server, error) {
	var r struct {
		Server Server `json:"server"`
	}
	return r.Server, c.do(ctx, http.MethodGet, "/api/v1/servers/"+id, nil, &r)
}

func (c *Client) Servers(ctx context.Context) ([]Server, error) {
	var r struct {
		Servers []Server `json:"servers"`
	}
	return r.Servers, c.do(ctx, http.MethodGet, "/api/v1/servers", nil, &r)
}

// Delete removes the server with its disk and IP. Deleting a server that is
// already gone is not an error: both ends of a session may try.
func (c *Client) Delete(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/api/v1/servers/"+id, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// PowerOn turns the server on and waits for the task. A server can be Active
// with its power off (the API's own sample shows it), and SSH never answers
// then.
func (c *Client) PowerOn(ctx context.Context, id string) error {
	var r struct {
		TaskID string `json:"task_id"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/servers/"+id+"/power/on", nil, &r); err != nil {
		return err
	}
	if r.TaskID == "" {
		return nil
	}
	_, err := c.WaitTask(ctx, r.TaskID, time.Second)
	return err
}

func (c *Client) Balance(ctx context.Context) (Balance, error) {
	var r struct {
		Project struct {
			Balance  float64 `json:"balance"`
			Currency string  `json:"currency"`
		} `json:"project"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/project", nil, &r)
	return Balance{Amount: r.Project.Balance, Currency: r.Project.Currency}, err
}

// APIError carries the HTTP status and whatever message the API returned.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("serverspace: %d %s", e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-KEY", c.Key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: errorMessage(data)}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

// errorMessage pulls messages out of {"errors":[{"message":...}]} and falls
// back to the raw body, since the error schema is not documented in detail.
func errorMessage(data []byte) string {
	var e struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &e) == nil && len(e.Errors) > 0 {
		msgs := make([]string, 0, len(e.Errors))
		for _, m := range e.Errors {
			msgs = append(msgs, m.Message)
		}
		return strings.Join(msgs, "; ")
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
