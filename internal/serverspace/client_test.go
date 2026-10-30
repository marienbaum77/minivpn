package serverspace

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateWaitDelete(t *testing.T) {
	var created map[string]any
	polls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/servers", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewDecoder(r.Body).Decode(&created)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"task_id":"lt1"}`))
	})
	mux.HandleFunc("GET /api/v1/tasks/lt1", func(w http.ResponseWriter, r *http.Request) {
		polls++
		state := "InProgress"
		if polls > 1 {
			state = "Completed"
		}
		w.Write([]byte(`{"task":{"id":"lt1","is_completed":"` + state + `","server_id":"l9s42","resources":[]}}`))
	})
	mux.HandleFunc("GET /api/v1/servers/l9s42", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"server":{"id":"l9s42","name":"minivpn-x","state":"Active","is_power_on":true,
			"nics":[{"network_type":"Isolated","ip_address":"10.0.0.2"},{"network_type":"PublicShared","ip_address":"203.0.113.7"}]}}`))
	})
	mux.HandleFunc("DELETE /api/v1/servers/l9s42", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"task_id":"lt2"}`))
	})
	mux.HandleFunc("DELETE /api/v1/servers/l9s43", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	ctx := context.Background()

	task, err := c.Create(ctx, Spec{Name: "minivpn-x", LocationID: "nj3", ImageID: "ubuntu", CPU: 1, RAMMB: 1024, DiskMB: 10240, BandwidthMbps: 50})
	if err != nil {
		t.Fatal(err)
	}
	if _, sent := created["server_init_script"]; sent || created["location_id"] != "nj3" {
		t.Fatalf("create body: %v", created)
	}
	vol := created["volumes"].([]any)[0].(map[string]any)
	if vol["name"] != "boot" || vol["size_mb"].(float64) != 10240 {
		t.Fatalf("boot volume: %v", vol)
	}

	done, err := c.WaitTask(ctx, task, time.Millisecond)
	if err != nil || done.ServerID != "l9s42" {
		t.Fatalf("wait: %+v %v", done, err)
	}
	s, err := c.Server(ctx, done.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if s.PublicIP() != "203.0.113.7" {
		t.Fatalf("public ip = %q", s.PublicIP())
	}
	if err := c.Delete(ctx, "l9s42"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "l9s43"); err != nil {
		t.Fatalf("deleting a gone server must succeed: %v", err)
	}
}

func TestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tasks/failed":
			w.Write([]byte(`{"task":{"id":"failed","is_completed":"Failed"}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"errors":[{"message":"not enough money"}]}`))
		}
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL

	_, err := c.Create(context.Background(), Spec{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "not enough money" {
		t.Fatalf("want APIError with message, got %v", err)
	}
	if _, err := c.WaitTask(context.Background(), "failed", time.Millisecond); err == nil {
		t.Fatal("failed task must be an error")
	}
}

func TestMinimalSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/locations":
			w.Write([]byte(`{"locations":[
				{"id":"am2","allow_creating_servers":false},
				{"id":"nj3","system_volume_min":10240,"bandwidth_min":10,"cpu_quantity_options":[2,1,4],"ram_size_options":[512,2048,1024],"allow_creating_servers":true}]}`))
		case "/api/v1/images":
			if r.URL.Query().Get("location_id") != "nj3" {
				t.Errorf("images for %q", r.URL.Query().Get("location_id"))
			}
			w.Write([]byte(`{"images":[
				{"id":"u2604","type":"Ubuntu","os_version":"26.04","architecture":"X64"},
				{"id":"u2510","type":"Ubuntu","os_version":"25.10","architecture":"X64"},
				{"id":"u2204","type":"Ubuntu","os_version":"22.04","architecture":"X64"},
				{"id":"u2404","type":"Ubuntu","os_version":"Ubuntu 24.04 LTS","architecture":"X64"},
				{"id":"d13","type":"Debian","os_version":"13","architecture":"X64"}]}`))
		}
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL

	got, err := c.MinimalSpec(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	want := Spec{LocationID: "nj3", ImageID: "u2404", CPU: 1, RAMMB: 1024, DiskMB: 10240, BandwidthMbps: 50}
	if got.LocationID != want.LocationID || got.ImageID != want.ImageID || got.CPU != want.CPU || got.RAMMB != want.RAMMB || got.DiskMB != want.DiskMB || got.BandwidthMbps != want.BandwidthMbps {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := c.MinimalSpec(context.Background(), "am2"); err == nil {
		t.Fatal("a location closed for new servers must be an error")
	}
}

func TestFallbackSpec(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/locations":
			w.Write([]byte(`{"locations":[
				{"id":"nj3","system_volume_min":10240,"bandwidth_min":10,"cpu_quantity_options":[1],"ram_size_options":[1024],"allow_creating_servers":false},
				{"id":"ca","system_volume_min":10240,"bandwidth_min":10,"cpu_quantity_options":[1],"ram_size_options":[1024],"allow_creating_servers":true}]}`))
		case "/api/v1/images":
			if r.URL.Query().Get("location_id") != "ca" {
				t.Errorf("images for %q", r.URL.Query().Get("location_id"))
			}
			w.Write([]byte(`{"images":[{"id":"u2404","type":"Ubuntu","os_version":"24.04","architecture":"X64"}]}`))
		}
	}))
	defer srv.Close()
	c := New("k")
	c.BaseURL = srv.URL

	got, err := c.FallbackSpec(context.Background(), "nj3")
	if err != nil {
		t.Fatal(err)
	}
	if got.LocationID != "ca" || got.ImageID != "u2404" || got.CPU != 1 || got.RAMMB != 1024 {
		t.Fatalf("got %+v", got)
	}
	if _, err := c.FallbackSpec(context.Background(), "ca"); err == nil || !strings.Contains(err.Error(), "no location other than ca") {
		t.Fatalf("want no fallback when no other location is open, got %v", err)
	}
}

func TestPickImageRequiresVerifiedUbuntu(t *testing.T) {
	_, err := pickImage([]Image{
		{ID: "d13", Type: "Debian", OSVersion: "13", Architecture: "X64"},
		{ID: "u2404-arm", Type: "Ubuntu", OSVersion: "24.04", Architecture: "ARM64"},
	})
	if err == nil || !strings.Contains(err.Error(), "no Ubuntu x64 image") {
		t.Fatalf("want explicit error without an Ubuntu x64 image, got %v", err)
	}
}

func TestPickImageFallsBackToUbuntu2204(t *testing.T) {
	got, err := pickImage([]Image{
		{ID: "u2604", Type: "Ubuntu", OSVersion: "26.04", Architecture: "X64"},
		{ID: "u2204", Type: "Ubuntu", OSVersion: "22.04", Architecture: "X64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "u2204" {
		t.Fatalf("selected image %q, want Ubuntu 22.04 image u2204", got.ID)
	}
}

func TestPickImageFallsBackToAvailableUbuntu(t *testing.T) {
	got, err := pickImage([]Image{
		{ID: "u2604", Type: "Ubuntu", OSVersion: "26.04 LTS", Architecture: "X64"},
		{ID: "u2510", Type: "Ubuntu", OSVersion: "25.10", Architecture: "X64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "u2604" {
		t.Fatalf("selected image %q, want first available Ubuntu x64 image u2604", got.ID)
	}
}

func TestPickLocationFallsBack(t *testing.T) {
	// nj3 is full today. Toronto is the same money and abroad as well, so the
	// default walks over to it instead of losing the session.
	abroad := []Location{
		{ID: "ix1", AllowCreating: true}, // Moscow: a node here unblocks nothing
		{ID: "nj3"},
		{ID: "ca", AllowCreating: true},
		{ID: "uae", AllowCreating: true}, // seven times the price
	}
	got, err := pickLocation(abroad, "")
	if err != nil || got.ID != "ca" {
		t.Fatalf("default picked %q, err %v", got.ID, err)
	}
	// A second New Jersey zone is found by the nj* prefix without editing code.
	got, err = pickLocation([]Location{{ID: "ca", AllowCreating: true}, {ID: "nj4", AllowCreating: true}}, "")
	if err != nil || got.ID != "nj4" {
		t.Fatalf("New Jersey beats Toronto: %q err %v", got.ID, err)
	}
	// An explicit choice is never swapped for another one, but the error says
	// what is open so there is something to set.
	if _, err := pickLocation(abroad, "nj3"); err == nil {
		t.Fatal("an explicit full location must stay an error")
	} else if !strings.Contains(err.Error(), "ix1, ca, uae") {
		t.Fatalf("error should name what is open: %v", err)
	}
	// Nothing we default to exists at all: list what does.
	if _, err := pickLocation([]Location{{ID: "kz", AllowCreating: true}}, ""); err == nil {
		t.Fatal("no New Jersey and no Toronto must be an error")
	} else if !strings.Contains(err.Error(), "kz") {
		t.Fatalf("error should list available ids: %v", err)
	}
	// Open nowhere: say so instead of listing an empty set.
	if _, err := pickLocation([]Location{{ID: "nj3"}, {ID: "ca"}}, ""); err == nil ||
		!strings.Contains(err.Error(), "no location is open") {
		t.Fatalf("want all-full error, got %v", err)
	}
}
