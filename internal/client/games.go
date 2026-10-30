package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DetectGameDirs finds game library folders: Steam libraries on every disk,
// Epic install locations, Riot and GOG defaults. Missing launchers are
// skipped. steamPath comes from the registry on Windows ("" if unknown).
func DetectGameDirs(steamPath string) []string {
	var dirs []string
	if steamPath != "" {
		if b, err := os.ReadFile(filepath.Join(steamPath, "steamapps", "libraryfolders.vdf")); err == nil {
			for _, lib := range SteamLibraries(string(b)) {
				dirs = append(dirs, filepath.Join(lib, "steamapps", "common"))
			}
		}
		dirs = append(dirs, filepath.Join(steamPath, "steamapps", "common"))
	}
	programData := os.Getenv("ProgramData")
	if programData != "" {
		dirs = append(dirs, EpicInstallDirs(filepath.Join(programData, "Epic", "EpicGamesLauncher", "Data", "Manifests"))...)
	}
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
		if pf := os.Getenv(env); pf != "" {
			dirs = append(dirs, filepath.Join(pf, "Epic Games"), filepath.Join(pf, "GOG Galaxy", "Games"))
		}
	}
	dirs = append(dirs, `C:\Riot Games`, `C:\GOG Games`)

	var out []string
	for _, d := range dirs {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

var vdfPath = regexp.MustCompile(`(?m)^\s*"path"\s*"((?:[^"\\]|\\.)*)"`)

// SteamLibraries pulls library roots out of libraryfolders.vdf.
func SteamLibraries(vdf string) []string {
	var out []string
	for _, m := range vdfPath.FindAllStringSubmatch(vdf, -1) {
		out = append(out, strings.ReplaceAll(m[1], `\\`, `\`))
	}
	return out
}

// EpicInstallDirs reads the launcher's *.item manifests. Each game can live
// anywhere, so the game folder itself is returned, not a common parent.
func EpicInstallDirs(manifests string) []string {
	files, _ := filepath.Glob(filepath.Join(manifests, "*.item"))
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m struct {
			InstallLocation string `json:"InstallLocation"`
		}
		if json.Unmarshal(b, &m) == nil && m.InstallLocation != "" {
			out = append(out, m.InstallLocation)
		}
	}
	return out
}
