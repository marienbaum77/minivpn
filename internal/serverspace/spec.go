package serverspace

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// FreeBandwidth is what Serverspace includes for free in New Jersey,
// Amsterdam and Toronto; more is billed per Mbit/s.
const FreeBandwidth = 50

// defaultLocations is the order the app tries when settings leave the
// location empty. New Jersey is the plan; Toronto is the same 0,61 ₽/h with
// the same free 50 Mbit/s, so a full New Jersey costs a session nothing but
// the address. The clusters left out here are either 1,8 to 7 times dearer or
// in a country worth thinking about first — that is a choice for settings,
// not something to fall into on its own.
//
// An entry ending in * matches any zone of that cluster, so a second New
// Jersey zone works without changing this list.
var defaultLocations = []string{"nj*", "ca"}

// MinimalSpec asks the API for the cheapest server that runs a node: the
// smallest CPU and at least 1 GB of RAM, the minimal disk, a verified Ubuntu
// LTS image, and the free bandwidth. location may be empty for defaultLocations.
func (c *Client) MinimalSpec(ctx context.Context, location string) (Spec, error) {
	locs, err := c.Locations(ctx)
	if err != nil {
		return Spec{}, err
	}
	loc, err := pickLocation(locs, location)
	if err != nil {
		return Spec{}, err
	}
	return c.specIn(ctx, loc)
}

// FallbackSpec picks a spec in a location other than avoid, keeping the
// default price order. Serverspace's public address pools are per location
// and it hands a freed address straight back to the next server; when one
// address is filtered, another location is the way to get a different one.
func (c *Client) FallbackSpec(ctx context.Context, avoid string) (Spec, error) {
	locs, err := c.Locations(ctx)
	if err != nil {
		return Spec{}, err
	}
	loc, err := pickLocationExcept(locs, defaultLocations, avoid)
	if err != nil {
		return Spec{}, fmt.Errorf("no location other than %s: %w", avoid, err)
	}
	return c.specIn(ctx, loc)
}

func (c *Client) specIn(ctx context.Context, loc Location) (Spec, error) {
	images, err := c.Images(ctx, loc.ID)
	if err != nil {
		return Spec{}, err
	}
	img, err := pickImage(images)
	if err != nil {
		return Spec{}, fmt.Errorf("location %s: %w", loc.ID, err)
	}
	return specFor(loc, img), nil
}

// pickLocation walks wants in order and returns the first location that
// still takes new servers, so a full cluster costs a session nothing but a
// different IP. When all of them are full the error names what is open,
// because that is what to set location to.
func pickLocation(locs []Location, want string) (Location, error) {
	wants := []string{want}
	if want == "" {
		wants = defaultLocations
	}
	return pickLocationExcept(locs, wants, "")
}

// pickLocationExcept is pickLocation with one location left out: a second
// node must not land in the cluster whose addresses just proved unreachable.
func pickLocationExcept(locs []Location, wants []string, avoid string) (Location, error) {
	var ids, open, full []string
	for _, l := range locs {
		ids = append(ids, l.ID)
		if l.AllowCreating {
			open = append(open, l.ID)
		}
	}
	for _, w := range wants {
		for _, l := range locs {
			if l.ID == avoid || !matchLocation(l.ID, w) {
				continue
			}
			if l.AllowCreating {
				return l, nil
			}
			full = append(full, l.ID)
		}
	}
	if len(full) > 0 {
		msg := "location " + strings.Join(full, ", ") + " does not accept new servers right now"
		if len(open) > 0 {
			return Location{}, fmt.Errorf("%s; locations that do: %s", msg, strings.Join(open, ", "))
		}
		return Location{}, fmt.Errorf("%s; no location is open, try again later", msg)
	}
	return Location{}, fmt.Errorf("location %s not found; available: %s", strings.Join(wants, ", "), strings.Join(ids, ", "))
}

// matchLocation compares an id with a wanted one, where a trailing * stands
// for any suffix: "nj*" is nj3 and whatever else New Jersey gets later.
func matchLocation(id, want string) bool {
	if prefix, star := strings.CutSuffix(want, "*"); star {
		return strings.HasPrefix(id, prefix)
	}
	return id == want
}

// pickImage prefers Ubuntu 24.04 LTS and falls back to 22.04. If neither is
// listed, use an available Ubuntu x64 image rather than failing before node
// creation; the setup script validates the installed sing-box configuration.
func pickImage(images []Image) (Image, error) {
	for _, version := range []string{"24.04", "22.04"} {
		for _, im := range images {
			if isUbuntuX64(im) && hasVersion(im.OSVersion, version) {
				return im, nil
			}
		}
	}
	for _, im := range images {
		if isUbuntuX64(im) {
			return im, nil
		}
	}
	return Image{}, fmt.Errorf("no Ubuntu x64 image")
}

func isUbuntuX64(im Image) bool {
	return strings.EqualFold(im.Type, "Ubuntu") &&
		(strings.EqualFold(im.Architecture, "X64") ||
			strings.EqualFold(im.Architecture, "x86_64") ||
			strings.EqualFold(im.Architecture, "amd64"))
}

func hasVersion(value, want string) bool {
	for _, part := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == ',' || r == '(' || r == ')'
	}) {
		part = strings.TrimLeft(part, "vV")
		if part == want || strings.HasPrefix(part, want+".") {
			return true
		}
	}
	return false
}

func specFor(loc Location, img Image) Spec {
	cpu := 1
	if len(loc.CPUOptions) > 0 {
		cpu = slices.Min(loc.CPUOptions)
	}
	ram := 1024
	for _, r := range slices.Sorted(slices.Values(loc.RAMOptions)) {
		if r >= 1024 {
			ram = r
			break
		}
	}
	disk := loc.SystemVolumeMin
	if disk <= 0 {
		disk = 10240
	}
	bw := FreeBandwidth
	if loc.BandwidthMin > bw {
		bw = loc.BandwidthMin
	}
	return Spec{LocationID: loc.ID, ImageID: img.ID, CPU: cpu, RAMMB: ram, DiskMB: disk, BandwidthMbps: bw}
}
