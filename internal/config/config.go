package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type RDP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Domain   string `json:"domain"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	// TypeDelayMS is the keystroke delay used while typing the bootstrap line.
	// Slow or busy hosts need a larger value, otherwise characters get dropped.
	TypeDelayMS int `json:"type_delay_ms"`
}

// Forward maps a local port to a port on the remote host.
type Forward struct {
	LocalPort  int    `json:"local_port"`
	RemotePort int    `json:"remote_port"`
	RemoteHost string `json:"remote_host"`
}

// Target is one RDP connection and the set of ports it exposes locally.
type Target struct {
	Name        string    `json:"name"`
	RDP         RDP       `json:"rdp"`
	ChannelName string    `json:"channel_name"`
	ListenHost  string    `json:"listen_host"`
	RemoteHost  string    `json:"remote_host"`
	Forwards    []Forward `json:"forwards"`
}

type Config struct {
	Targets []Target `json:"targets"`
}

type legacy struct {
	RDP    json.RawMessage `json:"rdp"`
	Chrome json.RawMessage `json:"chrome"`
	Tunnel json.RawMessage `json:"tunnel"`
}

func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, fmt.Errorf("config %s not found", path)
		}
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	var old legacy
	_ = json.Unmarshal(raw, &old)
	if len(cfg.Targets) == 0 && (old.RDP != nil || old.Chrome != nil || old.Tunnel != nil) {
		return cfg, fmt.Errorf("config %s uses the old single-target schema; "+
			"wrap it in \"targets\": [ { rdp, channel_name, forwards } ] (the chrome block is gone)", path)
	}
	if len(cfg.Targets) == 0 {
		return cfg, fmt.Errorf("config %s has no targets", path)
	}
	if err := normalize(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func normalize(cfg *Config) error {
	names := map[string]bool{}
	locals := map[int]string{}
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		if t.Name == "" {
			t.Name = fmt.Sprintf("target%d", i+1)
		}
		if names[t.Name] {
			return fmt.Errorf("duplicate target name %q", t.Name)
		}
		names[t.Name] = true
		if t.RDP.Host == "" {
			return fmt.Errorf("%s: rdp.host is required", t.Name)
		}
		if t.RDP.Port == 0 {
			t.RDP.Port = 3389
		}
		if t.RDP.TypeDelayMS <= 0 {
			t.RDP.TypeDelayMS = 8
		}
		if t.RDP.Width == 0 {
			t.RDP.Width = 1280
		}
		if t.RDP.Height == 0 {
			t.RDP.Height = 800
		}
		if t.ChannelName == "" {
			t.ChannelName = "rdpdt"
		}
		if len(t.ChannelName) > 7 {
			return fmt.Errorf("%s: channel_name must be 1-7 characters", t.Name)
		}
		if t.ListenHost == "" {
			t.ListenHost = "127.0.0.1"
		}
		if t.RemoteHost == "" {
			t.RemoteHost = "127.0.0.1"
		}
		for j, f := range t.Forwards {
			if f.LocalPort <= 0 || f.LocalPort > 65535 || f.RemotePort <= 0 || f.RemotePort > 65535 {
				return fmt.Errorf("%s: forwards[%d] needs valid local_port and remote_port", t.Name, j)
			}
			if owner, ok := locals[f.LocalPort]; ok {
				return fmt.Errorf("localhost:%d is claimed by both %s and %s", f.LocalPort, owner, t.Name)
			}
			locals[f.LocalPort] = t.Name
			if f.RemoteHost == "" {
				t.Forwards[j].RemoteHost = t.RemoteHost
			}
		}
	}
	return nil
}
