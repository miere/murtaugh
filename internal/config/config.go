package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultRelativePath = ".config/murtaugh/slack.yaml"

type Config struct {
	Slack    SlackConfig     `yaml:"slack"`
	Commands []CommandConfig `yaml:"commands"`
}

type SlackConfig struct {
	AppToken string `yaml:"app_token"`
	BotToken string `yaml:"bot_token"`
	Debug    bool   `yaml:"debug"`
}

type CommandConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, defaultRelativePath), nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.Slack.AppToken) == "" {
		errs = append(errs, errors.New("slack.app_token is required"))
	}
	if strings.TrimSpace(c.Slack.BotToken) == "" {
		errs = append(errs, errors.New("slack.bot_token is required"))
	}
	for i, command := range c.Commands {
		if !strings.HasPrefix(strings.TrimSpace(command.Name), "/") {
			errs = append(errs, fmt.Errorf("commands[%d].name must start with /", i))
		}
	}
	return errors.Join(errs...)
}
