package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationRoleAndHubKeys(t *testing.T) {
	for _, tc := range []struct {
		name, config, role, loadError, keyError string
		autoMint                                bool
	}{
		{name: "default", role: "off"},
		{name: "case normalization", config: "[federation]\nrole = \"SPOKE\"", role: "spoke"},
		{name: "hub keyless", config: "[federation]\nrole = \"hub\"", role: "hub", keyError: "federation hub role requires an effective API key"},
		{name: "hub owner", config: "[federation]\nrole = \"hub\"\n[server]\napi_key = \"owner-key\"", role: "hub"},
		{name: "hub remote readers", config: "[federation]\nrole = \"hub\"\n[server]\napi_key = \"owner-key\"\n[[server.remote_clients]]\nclient_id = \"reader\"\napi_key_file = \"reader\"", role: "hub", keyError: "federation hub role refuses [[server.remote_clients]]"},
		{name: "hub keyless network remote readers", config: "[federation]\nrole = \"hub\"\n[server]\nbind_addr = \"0.0.0.0\"\n[[server.remote_clients]]\nclient_id = \"reader\"\napi_key_file = \"reader\"", role: "hub", keyError: "federation hub role refuses [[server.remote_clients]]", autoMint: true},
		{name: "hub keyless network auto mint", config: "[federation]\nrole = \"hub\"\n[server]\nbind_addr = \"0.0.0.0\"", role: "hub", autoMint: true},
		{name: "invalid", config: "[federation]\nrole = \"other\"", loadError: "invalid [federation] role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := writeRemoteClientConfig(t, tc.config, nil)
			cfg, err := Load(path, t.TempDir())
			if tc.loadError != "" {
				requirements.ErrorContains(err, tc.loadError)
				return
			}
			requirements.NoError(err)
			cfg.Data.DataDir = t.TempDir()
			assertions.Equal(tc.role, cfg.Federation.Role)
			err = cfg.ValidateServerKey()
			if tc.keyError != "" {
				requirements.ErrorContains(err, tc.keyError)
			} else {
				requirements.NoError(err)
			}
			if tc.autoMint {
				err = cfg.PrepareServerKey()
				if tc.keyError != "" {
					requirements.ErrorContains(err, tc.keyError)
					_, err = os.Stat(cfg.ServerKeyFilePath())
					assertions.ErrorIs(err, os.ErrNotExist)
				} else {
					requirements.NoError(err)
				}
			}
		})
	}
}

func TestFederationHubPolicy(t *testing.T) {
	cfg := NewDefaultConfig()
	p := cfg.Federation.Hub.Policy()
	require.NoError(t, p.Validate())
	digest, err := p.PolicyDigest()
	require.NoError(t, err)
	p.AllowedLevels = []string{"content", "envelope"}
	reordered, err := p.PolicyDigest()
	require.NoError(t, err)
	assert.Equal(t, digest, reordered)
	for _, change := range []func(*FederationHubPolicy){
		func(p *FederationHubPolicy) { p.Retention = "invalid" },
		func(p *FederationHubPolicy) { p.AllowedLevels = []string{"invalid"} },
		func(p *FederationHubPolicy) { p.MinGroupSize = 1 },
		func(p *FederationHubPolicy) { p.Processing = "" },
	} {
		bad := cfg.Federation.Hub.Policy()
		change(&bad)
		assert.Error(t, bad.Validate())
	}
	p.AllowAttachments = !p.AllowAttachments
	changed, err := p.PolicyDigest()
	require.NoError(t, err)
	assert.NotEqual(t, digest, changed)
}
