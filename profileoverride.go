package waxtap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/colespringer/waxtap/potoken"
	"github.com/colespringer/waxtap/youtube"
)

// profileOverrideFile is the JSON schema of a client-profile override file. It
// declares the full ordered client strategy chain, replacing the built-in
// defaults so a deployment can refresh client versions, user agents, or device
// fingerprints with a config change and a restart instead of a rebuild.
//
// Unknown keys are rejected because this file is usually used to repair stale
// clients quickly. A typo should fail startup instead of leaving the stale value
// in place.
type profileOverrideFile struct {
	Profiles []profileSpec `json:"profiles"`
}

// profileSpec is the JSON form of the client-profile fields WaxTap allows at
// runtime. requiresPoToken uses scope names ("none", "gvs", "player",
// "subtitles") rather than Go enum values.
type profileSpec struct {
	Name              string `json:"name"`
	InnerTubeName     string `json:"innerTubeName"`
	InnerTubeID       int    `json:"innerTubeId"`
	Version           string `json:"version"`
	APIKey            string `json:"apiKey"`
	UserAgent         string `json:"userAgent"`
	DeviceMake        string `json:"deviceMake"`
	DeviceModel       string `json:"deviceModel"`
	OSName            string `json:"osName"`
	OSVersion         string `json:"osVersion"`
	AndroidSDKVersion int    `json:"androidSdkVersion"`
	RequiresPOToken   string `json:"requiresPoToken"`
	SupportsCookies   bool   `json:"supportsCookies"`
	SupportsPlaylists bool   `json:"supportsPlaylists"`
}

// loadProfileOverrides reads a client-profile override file and returns the
// replacement strategy chain. The file is deliberately strict: malformed JSON,
// unknown fields, trailing data, an empty list, or an incomplete profile all fail
// instead of mixing a bad override with the built-ins.
func loadProfileOverrides(path string) ([]youtube.ClientProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile override %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f profileOverrideFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse profile override %s: %w", path, err)
	}
	// json.Decoder stops after the first value. Check for EOF so a second object
	// or stray bytes cannot be mistaken for applied configuration.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("profile override %s: unexpected trailing data after the JSON document", path)
	}
	if len(f.Profiles) == 0 {
		return nil, fmt.Errorf("profile override %s: no profiles defined", path)
	}

	profiles := make([]youtube.ClientProfile, 0, len(f.Profiles))
	for i, sp := range f.Profiles {
		// innerTubeId is part of the client identity, not optional metadata: it
		// becomes X-Youtube-Client-Name. Reject JSON's zero value for an omitted
		// field before it can produce a client name of "0".
		if sp.Name == "" || sp.InnerTubeName == "" || sp.Version == "" || sp.InnerTubeID <= 0 {
			return nil, fmt.Errorf("profile override %s: profile %d needs name, innerTubeName, version, and a positive innerTubeId", path, i)
		}
		scope, err := potoken.ParseScope(sp.RequiresPOToken)
		if err != nil {
			return nil, fmt.Errorf("profile override %s: profile %q: %w", path, sp.Name, err)
		}
		profiles = append(profiles, youtube.BuildProfile(youtube.ClientProfile{
			Name:              sp.Name,
			InnerTubeName:     sp.InnerTubeName,
			InnerTubeID:       sp.InnerTubeID,
			Version:           sp.Version,
			APIKey:            sp.APIKey,
			UserAgent:         sp.UserAgent,
			DeviceMake:        sp.DeviceMake,
			DeviceModel:       sp.DeviceModel,
			OSName:            sp.OSName,
			OSVersion:         sp.OSVersion,
			AndroidSDKVersion: sp.AndroidSDKVersion,
			RequiresPOToken:   scope,
			SupportsCookies:   sp.SupportsCookies,
			SupportsPlaylists: sp.SupportsPlaylists,
		}))
	}
	return profiles, nil
}
