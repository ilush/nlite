package main

import (
	"strings"
	"testing"
)

func TestExampleScenes(t *testing.T) {
	c, err := loadConfig("../../examples/scenes.json")
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]map[string]State{
		"video-calls": {
			"left":  {Power: "on", Brightness: 65, Kelvin: 4300},
			"right": {Power: "on", Brightness: 10, Kelvin: 4300},
		},
		"evening-ambient": {
			"left":  {Power: "on", Brightness: 5, Kelvin: 2900},
			"right": {Power: "off"},
		},
	}
	for scene, states := range expected {
		for light, state := range states {
			if c.Scenes[scene][light] != state {
				t.Errorf("%s/%s: got %+v, want %+v", scene, light, c.Scenes[scene][light], state)
			}
		}
	}
}

func TestConfigValidation(t *testing.T) {
	for _, state := range []State{
		{}, {Power: "ON"}, {Power: "off", Kelvin: 4300},
		{Power: "off", Brightness: 1},
		{Power: "on", Brightness: 0, Kelvin: 4300},
		{Power: "on", Brightness: 101, Kelvin: 4300},
		{Power: "on", Brightness: 5, Kelvin: 2899},
		{Power: "on", Brightness: 5, Kelvin: 7100},
		{Power: "on", Brightness: 5, Kelvin: 4350},
	} {
		if err := state.validate(); err == nil {
			t.Errorf("accepted invalid state %+v", state)
		}
	}
	for _, text := range []string{
		`{}`,
		`{"lights":{"left":"::1"},"scenes":{"off":{"left":{"power":"off"}}}}`,
		`{"lights":{"left":"0.0.0.0"},"scenes":{"off":{"left":{"power":"off"}}}}`,
		`{"lights":{"left":"127.0.0.1","right":"127.0.0.1"},"scenes":{"off":{"left":{"power":"off"}}}}`,
		`{"lights":{"left":"127.0.0.1"},"scenes":{"off":{"right":{"power":"off"}}}}`,
		`{"lights":{"left":"127.0.0.1"},"scenes":{"off":{}}}`,
	} {
		var c Config
		if err := readJSON(strings.NewReader(text), &c); err != nil {
			t.Fatal(err)
		}
		if err := c.validate(); err == nil {
			t.Errorf("accepted invalid config %s", text)
		}
	}
	for _, text := range []string{
		`{"unknown":true}`, `{} {}`, `{} trailing`,
		`{"scenes":{"off":{"left":{"power":"off","typo":1}}}}`,
	} {
		var c Config
		if err := readJSON(strings.NewReader(text), &c); err == nil {
			t.Errorf("accepted malformed/unknown JSON %s", text)
		}
	}
}
