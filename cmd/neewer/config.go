package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
)

type State struct {
	Power      string `json:"power"`
	Brightness int    `json:"brightness,omitempty"`
	Kelvin     int    `json:"kelvin,omitempty"`
}

type Config struct {
	Lights map[string]string           `json:"lights"`
	Scenes map[string]map[string]State `json:"scenes"`
}

func readJSON(r io.Reader, dst any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}

func loadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	if err := readJSON(f, &c); err != nil {
		return c, fmt.Errorf("config: %w", err)
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if len(c.Lights) == 0 || len(c.Scenes) == 0 {
		return errors.New("config needs lights and scenes")
	}
	seen := make(map[string]bool)
	for name, address := range c.Lights {
		ip := net.ParseIP(address)
		if name == "" || ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
			return fmt.Errorf("light %q needs a unicast IPv4 address", name)
		}
		key := ip.String()
		if seen[key] {
			return fmt.Errorf("duplicate light address %s", address)
		}
		seen[key] = true
	}
	for name, scene := range c.Scenes {
		if name == "" || len(scene) == 0 {
			return errors.New("scene names and scene contents must not be empty")
		}
		for light, state := range scene {
			if _, ok := c.Lights[light]; !ok {
				return fmt.Errorf("scene %q: unknown light %q", name, light)
			}
			if err := state.validate(); err != nil {
				return fmt.Errorf("scene %q, light %q: %w", name, light, err)
			}
		}
	}
	return nil
}

func (s State) validate() error {
	switch s.Power {
	case "off":
		if s.Brightness != 0 || s.Kelvin != 0 {
			return errors.New("off states must omit brightness and kelvin")
		}
	case "on":
		if s.Brightness < 1 || s.Brightness > 100 || s.Kelvin < 2900 || s.Kelvin > 7000 || s.Kelvin%100 != 0 {
			return errors.New("on states need brightness 1-100 and kelvin 2900-7000 in steps of 100")
		}
	default:
		return errors.New("power must be on or off")
	}
	return nil
}
