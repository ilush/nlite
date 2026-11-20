package main

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type receivedPacket struct {
	data   []byte
	at     time.Time
	source string
}

type fakeLight struct {
	conn    *net.UDPConn
	packets chan receivedPacket
}

func newFakeLight(t *testing.T) *fakeLight {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeLight{conn: conn, packets: make(chan receivedPacket, 256)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			buffer := make([]byte, 1024)
			n, source, err := conn.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			select {
			case f.packets <- receivedPacket{buffer[:n], time.Now(), source.String()}:
			case <-time.After(time.Second):
				return
			}
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		<-done
	})
	return f
}

func (f *fakeLight) take(t *testing.T, count int) []receivedPacket {
	t.Helper()
	packets := make([]receivedPacket, 0, count)
	for len(packets) < count {
		select {
		case p := <-f.packets:
			packets = append(packets, p)
		case <-time.After(5 * time.Second):
			t.Fatalf("received %d packets, wanted %d", len(packets), count)
		}
	}
	return packets
}

func attachFake(t *testing.T, l *light) *fakeLight {
	t.Helper()
	f := newFakeLight(t)
	l.address = f.conn.LocalAddr().String()
	l.status.Address = l.address
	return f
}

func quickTiming() timing {
	return timing{8 * time.Millisecond, 30 * time.Millisecond, time.Second, time.Minute}
}

func testConfig() Config {
	return Config{
		Lights: map[string]string{"left": "127.0.0.1", "right": "127.0.0.2"},
		Scenes: map[string]map[string]State{
			"on": {
				"left":  {Power: "on", Brightness: 65, Kelvin: 4300},
				"right": {Power: "on", Brightness: 10, Kelvin: 4300},
			},
			"off": {"left": {Power: "off"}, "right": {Power: "off"}},
		},
	}
}

func singleHelper(t *testing.T, timing timing) (*Helper, *fakeLight) {
	t.Helper()
	c := testConfig()
	delete(c.Lights, "right")
	for _, scene := range c.Scenes {
		delete(scene, "right")
	}
	h := newHelper(context.Background(), c, timing)
	f := attachFake(t, h.lights["left"])
	t.Cleanup(h.close)
	return h, f
}

func assertPackets(t *testing.T, packets []receivedPacket, expected ...string) {
	t.Helper()
	if len(packets) != len(expected) {
		t.Fatalf("got %d packets, want %d", len(packets), len(expected))
	}
	for i, packet := range packets {
		if got := hex.EncodeToString(packet.data); got != expected[i] {
			t.Errorf("packet %d: got %s, want %s", i, got, expected[i])
		}
	}
}

const loopbackHandshake = "80020c0000093132372e302e302e314c"

func TestColdAndWarmSessions(t *testing.T) {
	h, f := singleHelper(t, quickTiming())
	result, err := h.apply(context.Background(), "on")
	if err != nil || !result["left"].Sent {
		t.Fatalf("apply: %v, %+v", err, result)
	}
	cold := f.take(t, 6)
	assertPackets(t, cold, loopbackHandshake, loopbackHandshake, loopbackHandshake,
		"8006010188", "800502010189", "80050402412b3229")
	if _, err := h.apply(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	warm := f.take(t, 1)
	assertPackets(t, warm, "800502010088")
	if cold[0].source != warm[0].source {
		t.Fatal("warm session did not retain the UDP source port")
	}
	status := h.status()["left"]
	if status.LastRequest.Power != "off" || status.LastSceneSentAt.IsZero() || status.SourceIP != "127.0.0.1" {
		t.Fatalf("unexpected status %+v", status)
	}
}

func TestDefaultPacketSpacing(t *testing.T) {
	h, f := singleHelper(t, defaultTiming)
	start := time.Now()
	if _, err := h.apply(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 4*defaultTiming.delay {
		t.Fatalf("cold off sent too quickly: %v", elapsed)
	}
	packets := f.take(t, 5)
	for i := 1; i < len(packets); i++ {
		// Receiving timestamps include scheduler jitter, unlike the send deadline.
		if gap := packets[i].at.Sub(packets[i-1].at); gap < defaultTiming.delay-20*time.Millisecond {
			t.Errorf("packet %d spacing %v, expected about %v", i, gap, defaultTiming.delay)
		}
	}
}

func TestLightsInitializeConcurrently(t *testing.T) {
	h := newHelper(context.Background(), testConfig(), quickTiming())
	defer h.close()
	left := attachFake(t, h.lights["left"])
	right := attachFake(t, h.lights["right"])
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	for _, l := range h.lights {
		l.sourceIP = func(address string) (string, error) {
			entered <- struct{}{}
			select {
			case <-release:
				return routeSource(address)
			case <-time.After(time.Second):
				return "", errors.New("other light did not initialize concurrently")
			}
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.apply(context.Background(), "on")
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("lights initialized sequentially")
		}
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lp, rp := left.take(t, 6), right.take(t, 6)
	assertPackets(t, lp[5:], "80050402412b3229")
	assertPackets(t, rp[5:], "800504020a2b32f2")
}

func TestScenesDoNotInterleave(t *testing.T) {
	h, f := singleHelper(t, quickTiming())
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() {
		_, err := h.apply(context.Background(), "on")
		first <- err
	}()
	initial := f.take(t, 1)
	go func() {
		_, err := h.apply(context.Background(), "off")
		second <- err
	}()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	packets := append(initial, f.take(t, 6)...)
	assertPackets(t, packets, loopbackHandshake, loopbackHandshake, loopbackHandshake,
		"8006010188", "800502010189", "80050402412b3229", "800502010088")
}

func TestReinitializeAfterSleepRouteChangeAndExpiry(t *testing.T) {
	for _, reason := range []string{"sleep", "clock-backwards", "source-change", "renewal"} {
		t.Run(reason, func(t *testing.T) {
			h, f := singleHelper(t, quickTiming())
			if _, err := h.apply(context.Background(), "off"); err != nil {
				t.Fatal(err)
			}
			f.take(t, 5)
			l := h.lights["left"]
			switch reason {
			case "sleep":
				l.status.LastPacketSentAt = time.Now().Add(-2 * l.timing.stale)
			case "clock-backwards":
				l.status.LastPacketSentAt = time.Now().Add(time.Hour)
			case "source-change":
				l.status.SourceIP = "192.0.2.1"
			case "renewal":
				l.readyAt = time.Now().Add(-2 * l.timing.renew)
			}
			if reason == "clock-backwards" {
				// Reinitialization must not wait an hour for the old wall clock.
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := l.beat(ctx); err != nil {
					t.Fatal(err)
				}
			} else if err := l.beat(context.Background()); err != nil {
				t.Fatal(err)
			}
			packets := f.take(t, 5)
			assertPackets(t, packets, loopbackHandshake, loopbackHandshake, loopbackHandshake, "8006010188", "80040084")
			if l.status.SourceIP != "127.0.0.1" {
				t.Fatal("route source was not refreshed")
			}
		})
	}
}

func TestErrorsAreReportedAndNextSceneReconnects(t *testing.T) {
	h, f := singleHelper(t, quickTiming())
	if _, err := h.apply(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	f.take(t, 5)
	l := h.lights["left"]
	l.conn.Close()
	result, err := h.apply(context.Background(), "on")
	if err == nil || result["left"].Sent || result["left"].Error == "" || h.status()["left"].Error == "" {
		t.Fatalf("missing UDP error: %+v, %v", result, err)
	}
	if _, err := h.apply(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	assertPackets(t, f.take(t, 5), loopbackHandshake, loopbackHandshake, loopbackHandshake, "8006010188", "800502010088")
	if h.status()["left"].Error != "" {
		t.Fatal("error not cleared after successful reconnect")
	}
	l.sourceIP = func(string) (string, error) { return "", errors.New("route unavailable") }
	result, err = h.apply(context.Background(), "on")
	if err == nil || result["left"].Sent || l.conn != nil {
		t.Fatalf("missing route failure: %+v, %v", result, err)
	}
}

func TestCanceledSceneNeverReplays(t *testing.T) {
	slow := quickTiming()
	slow.delay = 30 * time.Millisecond
	h, f := singleHelper(t, slow)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.apply(ctx, "on")
		done <- err
	}()
	f.take(t, 1)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("canceled scene reported success")
	}
	if _, err := h.apply(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	assertPackets(t, f.take(t, 5), loopbackHandshake, loopbackHandshake, loopbackHandshake, "8006010188", "800502010088")
	if _, err := h.apply(context.Background(), "missing"); err == nil {
		t.Fatal("unknown scene accepted")
	}
}

func TestBackgroundHeartbeatAndShutdown(t *testing.T) {
	h, f := singleHelper(t, quickTiming())
	h.start()
	select {
	case p := <-f.packets:
		t.Fatalf("helper sent a packet before a scene: %x", p.data)
	case <-time.After(2 * quickTiming().heartbeat):
	}
	if _, err := h.apply(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	f.take(t, 5)
	assertPackets(t, f.take(t, 1), "80040084")
	h.close()
	if h.lights["left"].conn != nil {
		t.Fatal("shutdown left UDP socket open")
	}
}

func TestHeartbeatCannotSplitScene(t *testing.T) {
	timing := quickTiming()
	timing.heartbeat = time.Millisecond
	h, f := singleHelper(t, timing)
	h.start()
	if _, err := h.apply(context.Background(), "on"); err != nil {
		t.Fatal(err)
	}
	assertPackets(t, f.take(t, 6), loopbackHandshake, loopbackHandshake, loopbackHandshake,
		"8006010188", "800502010189", "80050402412b3229")
	assertPackets(t, f.take(t, 1), "80040084")
}

func TestCanceledQueuedSceneIsDiscarded(t *testing.T) {
	h, f := singleHelper(t, quickTiming())
	h.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.apply(ctx, "on"); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation returned %v", err)
	}
	<-h.gate
	if _, err := h.apply(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	assertPackets(t, f.take(t, 5), loopbackHandshake, loopbackHandshake, loopbackHandshake,
		"8006010188", "800502010088")
}
