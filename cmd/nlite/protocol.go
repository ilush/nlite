package main

import (
	"fmt"
	"net"
)

func checksum(body []byte) []byte {
	var sum byte
	for _, b := range body {
		sum += b
	}
	return append(body, sum)
}

func handshake(source string) ([]byte, error) {
	ip := net.ParseIP(source)
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("not an IPv4 source address: %q", source)
	}
	text := []byte(ip.String())
	body := append([]byte{0x80, 0x02, byte(len(text) + 3), 0, 0, byte(len(text))}, text...)
	return checksum(body), nil
}

func statePackets(s State) [][]byte {
	if s.Power == "off" {
		return [][]byte{{0x80, 0x05, 0x02, 0x01, 0x00, 0x88}}
	}
	return [][]byte{
		{0x80, 0x05, 0x02, 0x01, 0x01, 0x89},
		checksum([]byte{0x80, 0x05, 0x04, 0x02, byte(s.Brightness), byte(s.Kelvin / 100), 0x32}),
	}
}
