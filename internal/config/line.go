package config

import (
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/envutil"
)

// LineSource configures a push source. The secret itself remains in the environment.
type LineSource struct {
	ChannelID string `toml:"channel_id"`
	SecretEnv string `toml:"secret_env"`
	Enabled   bool   `toml:"enabled"`
	AllowSend bool   `toml:"allow_send"`
}

func (c *Config) GetLineSource(channelID string) *LineSource {
	for _, source := range c.Line {
		if source.ChannelID == channelID {
			return &source
		}
	}
	return nil
}

func (c *Config) validateLineSources() error {
	seen := make(map[string]bool)
	for index := range c.Line {
		source := &c.Line[index]
		source.ChannelID = strings.TrimSpace(source.ChannelID)
		id, err := strconv.ParseUint(source.ChannelID, 10, 64)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != source.ChannelID {
			return fmt.Errorf("line[%d].channel_id must be a canonical positive decimal uint64", index)
		}
		if seen[source.ChannelID] {
			return fmt.Errorf("line[%d].channel_id is duplicate", index)
		}
		seen[source.ChannelID] = true
		source.SecretEnv = strings.TrimSpace(source.SecretEnv)
		if !envutil.ValidName(source.SecretEnv) {
			return fmt.Errorf("line[%d].secret_env must name an explicit environment variable", index)
		}
	}
	return nil
}
