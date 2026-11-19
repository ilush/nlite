package main

import (
	"encoding/hex"
	"testing"
)

func TestHandshake(t *testing.T) {
	for _, tt := range []struct {
		source string
		hex    string
	}{
		{"192.168.1.183", "80021000000d3139322e3136382e312e31383331"},
		{"10.0.0.1", "80020b00000831302e302e302e3111"},
		{"192.168.100.200", "80021200000f3139322e3136382e3130302e3230308b"},
	} {
		t.Run(tt.source, func(t *testing.T) {
			packet, err := handshake(tt.source)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(packet); got != tt.hex {
				t.Fatalf("got %s, want %s", got, tt.hex)
			}
			if int(packet[2]) != len(tt.source)+3 || int(packet[5]) != len(tt.source) {
				t.Fatalf("bad address lengths: %x", packet)
			}
		})
	}
	for _, bad := range []string{"", "::1", "not-an-ip"} {
		if _, err := handshake(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestStatePackets(t *testing.T) {
	for _, tt := range []struct {
		state State
		hex   []string
	}{
		{State{Power: "off"}, []string{"800502010088"}},
		{State{Power: "on", Brightness: 65, Kelvin: 4300}, []string{"800502010189", "80050402412b3229"}},
		{State{Power: "on", Brightness: 10, Kelvin: 4300}, []string{"800502010189", "800504020a2b32f2"}},
		{State{Power: "on", Brightness: 5, Kelvin: 2900}, []string{"800502010189", "80050402051d32df"}},
	} {
		packets := statePackets(tt.state)
		if len(packets) != len(tt.hex) {
			t.Fatal("wrong packet count")
		}
		for i, packet := range packets {
			if got := hex.EncodeToString(packet); got != tt.hex[i] {
				t.Errorf("packet %d: got %s, want %s", i, got, tt.hex[i])
			}
		}
	}
}
