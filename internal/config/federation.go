package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

// FederationConfig selects this archive's federation role and hub policy.
type FederationConfig struct {
	Role string              `toml:"role"`
	Hub  FederationHubPolicy `toml:"hub"`
}

type FederationHubPolicy struct {
	Retention        string   `toml:"retention" json:"retention"`
	AllowedLevels    []string `toml:"allowed_levels" json:"allowed_levels"`
	AllowAttachments bool     `toml:"allow_attachments" json:"allow_attachments"`
	MinGroupSize     int      `toml:"min_group_size" json:"min_group_size"`
	Processing       string   `toml:"processing" json:"processing"`
}


func (p FederationHubPolicy) Validate() error {
	if p.Retention != "member-controlled" && p.Retention != "org-retained" {
		return fmt.Errorf("invalid federation hub retention")
	}
	if len(p.AllowedLevels) == 0 {
		return fmt.Errorf("federation hub allowed_levels must not be empty")
	}
	for _, level := range p.AllowedLevels {
		if level != "envelope" && level != "content" {
			return fmt.Errorf("invalid federation hub allowed_levels")
		}
	}
	if p.MinGroupSize < 1 {
		return fmt.Errorf("federation hub min_group_size must be at least 1")
	}
	return nil
}

func (p FederationHubPolicy) PolicyDigest() (string, error) {
	p.AllowedLevels = slices.Clone(p.AllowedLevels)
	slices.Sort(p.AllowedLevels)
	b, err := json.Marshal(p, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (f *FederationConfig) Validate() error {
	f.Role = strings.ToLower(f.Role)
	if f.Role == "" {
		f.Role = "off"
	}
	switch f.Role {
	case "off", "spoke":
		return nil
	case "hub":
		return f.Hub.Validate()
	default:
		return fmt.Errorf("invalid [federation] role %q", f.Role)
	}
}
