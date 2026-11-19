package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

type timing struct {
	delay     time.Duration
	heartbeat time.Duration
	stale     time.Duration
	renew     time.Duration
}

var defaultTiming = timing{500 * time.Millisecond, 2 * time.Second, 6 * time.Second, time.Minute}

type LightStatus struct {
	Address          string    `json:"address"`
	SourceIP         string    `json:"source_ip,omitempty"`
	LastPacketSentAt time.Time `json:"last_packet_sent_at"`
	LastSceneSentAt  time.Time `json:"last_scene_sent_at"`
	LastRequest      *State    `json:"last_request,omitempty"`
	Error            string    `json:"error,omitempty"`
}

type light struct {
	mu       sync.Mutex
	address  string
	conn     *net.UDPConn
	status   LightStatus
	readyAt  time.Time
	timing   timing
	sourceIP func(string) (string, error)
}

// A probe asks the kernel for the route's source address without sending a datagram.
func routeSource(address string) (string, error) {
	remote, err := net.ResolveUDPAddr("udp4", address)
	if err != nil {
		return "", err
	}
	probe, err := net.DialUDP("udp4", nil, remote)
	if err != nil {
		return "", err
	}
	defer probe.Close()
	return probe.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

func wallAge(now, then time.Time) time.Duration {
	// Wall time detects sleep even on platforms whose monotonic clock pauses.
	return time.Duration(now.UnixNano() - then.UnixNano())
}

func (l *light) disconnect() {
	if l.conn != nil {
		l.conn.Close()
		l.conn = nil
	}
	l.readyAt = time.Time{}
	l.status.SourceIP = ""
}

func (l *light) fail(err error) error {
	l.disconnect()
	l.status.Error = err.Error()
	return err
}

func (l *light) send(ctx context.Context, packet []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var remaining time.Duration
	if !l.status.LastPacketSentAt.IsZero() {
		age := wallAge(time.Now(), l.status.LastPacketSentAt)
		if age < 0 {
			age = 0
		}
		remaining = l.timing.delay - age
	}
	if remaining > 0 {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := l.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	n, err := l.conn.Write(packet)
	if err != nil {
		return err
	}
	if n != len(packet) {
		return errors.New("short UDP write")
	}
	l.status.LastPacketSentAt = time.Now()
	return nil
}

func (l *light) connect(ctx context.Context) error {
	source, err := l.sourceIP(l.address)
	if err != nil {
		return fmt.Errorf("route to %s: %w", l.address, err)
	}
	now := time.Now()
	gap := wallAge(now, l.status.LastPacketSentAt)
	age := wallAge(now, l.readyAt)
	if l.conn != nil && source == l.status.SourceIP && gap >= 0 && gap < l.timing.stale && age >= 0 && age < l.timing.renew {
		return nil
	}
	l.disconnect()
	remote, err := net.ResolveUDPAddr("udp4", l.address)
	if err != nil {
		return err
	}
	packet, err := handshake(source)
	if err != nil {
		return err
	}
	l.conn, err = net.DialUDP("udp4", &net.UDPAddr{IP: net.ParseIP(source)}, remote)
	if err != nil {
		return err
	}
	l.status.SourceIP = source
	for _, p := range [][]byte{packet, packet, packet, {0x80, 0x06, 0x01, 0x01, 0x88}} {
		if err := l.send(ctx, p); err != nil {
			return err
		}
	}
	l.readyAt = time.Now()
	return nil
}

func (l *light) apply(ctx context.Context, s State) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status.LastRequest = &s
	if err := l.connect(ctx); err != nil {
		return l.fail(err)
	}
	for _, p := range statePackets(s) {
		if err := l.send(ctx, p); err != nil {
			return l.fail(err)
		}
	}
	l.status.LastSceneSentAt = time.Now()
	l.status.Error = ""
	return nil
}

func (l *light) beat(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		// Failed sessions retry on the next scene, never replay old scene state.
		return nil
	}
	if err := l.connect(ctx); err != nil {
		return l.fail(err)
	}
	if err := l.send(ctx, []byte{0x80, 0x04, 0x00, 0x84}); err != nil {
		return l.fail(err)
	}
	l.status.Error = ""
	return nil
}

type Helper struct {
	config Config
	lights map[string]*light
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	gate   chan struct{}
}

func newHelper(parent context.Context, c Config, t timing) *Helper {
	ctx, cancel := context.WithCancel(parent)
	h := &Helper{config: c, lights: make(map[string]*light), ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1)}
	for name, ip := range c.Lights {
		address := net.JoinHostPort(ip, "5052")
		h.lights[name] = &light{address: address, status: LightStatus{Address: address}, timing: t, sourceIP: routeSource}
	}
	return h
}

func (h *Helper) start() {
	for name, l := range h.lights {
		h.wg.Add(1)
		go func(name string, l *light) {
			defer h.wg.Done()
			ticker := time.NewTicker(l.timing.heartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-h.ctx.Done():
					return
				case <-ticker.C:
					if err := l.beat(h.ctx); err != nil && h.ctx.Err() == nil {
						log.Printf("light %s heartbeat failed: %v", name, err)
					}
				}
			}
		}(name, l)
	}
}

func (h *Helper) close() {
	h.cancel()
	h.wg.Wait()
	for _, l := range h.lights {
		l.mu.Lock()
		l.disconnect()
		l.mu.Unlock()
	}
}

type SceneResult struct {
	Sent  bool   `json:"sent"`
	Error string `json:"error,omitempty"`
}

func (h *Helper) apply(ctx context.Context, name string) (map[string]SceneResult, error) {
	scene, ok := h.config.Scenes[name]
	if !ok {
		return nil, fmt.Errorf("unknown scene %q", name)
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(h.ctx, cancel)
	defer stop()
	defer cancel()
	select {
	case h.gate <- struct{}{}:
		defer func() { <-h.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	results := make(map[string]SceneResult)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, s := range scene {
		wg.Add(1)
		go func(name string, s State) {
			defer wg.Done()
			result := SceneResult{Sent: true}
			if err := h.lights[name].apply(ctx, s); err != nil {
				result.Sent = false
				result.Error = err.Error()
				log.Printf("scene light %s failed: %v", name, err)
			}
			mu.Lock()
			results[name] = result
			mu.Unlock()
		}(name, s)
	}
	wg.Wait()
	for _, r := range results {
		if !r.Sent {
			return results, errors.New("scene partially or wholly failed; light state is unknown")
		}
	}
	return results, nil
}

func (h *Helper) status() map[string]LightStatus {
	result := make(map[string]LightStatus)
	for name, l := range h.lights {
		l.mu.Lock()
		result[name] = l.status
		l.mu.Unlock()
	}
	return result
}
