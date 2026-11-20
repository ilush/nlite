package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ng-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "control.sock")
}

func TestLocalSocketPermissionsAndExclusion(t *testing.T) {
	path := socketPath(t)
	listener, err := listenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %o", info.Mode().Perm())
	}
	if other, err := listenLocal(path); err == nil {
		other.Close()
		t.Fatal("second helper acquired the same socket")
	}
	listener.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket not removed at shutdown: %v", err)
	}
	restarted, err := listenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if other, err := listenLocal(path); err == nil {
		other.Close()
		t.Fatal("accepted non-private socket directory")
	}
}

func TestStaleSocketRecoveryAndNonSocketSafety(t *testing.T) {
	path := socketPath(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()
	restarted, err := listenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Close()
	if err := os.WriteFile(path, []byte("preserve me"), 0600); err != nil {
		t.Fatal(err)
	}
	if other, err := listenLocal(path); err == nil {
		other.Close()
		t.Fatal("replaced a regular file")
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "preserve me" {
		t.Fatalf("regular file was changed: %q, %v", content, err)
	}
}

func TestHTTPShutdownRetainsLifetimeLock(t *testing.T) {
	path := socketPath(t)
	listener, err := listenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.NewServeMux(), ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener.UnixListener) }()
	server.Close()
	<-done
	if other, err := listenLocal(path); err == nil {
		other.Close()
		t.Fatal("HTTP shutdown released the helper lifetime lock too soon")
	}
	listener.Close()
	restarted, err := listenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted.Close()
}

func TestExistingUnmanagedSocketIsNotReplaced(t *testing.T) {
	path := socketPath(t)
	original, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if other, err := listenLocal(path); err == nil {
		other.Close()
		t.Fatal("replaced a live socket")
	}
}

func TestHTTPValidation(t *testing.T) {
	h, _ := singleHelper(t, quickTiming())
	for _, tt := range []struct {
		method string
		path   string
		body   string
		code   int
	}{
		{"GET", "/status", "", 200},
		{"POST", "/scene", `{"name":"missing"}`, 404},
		{"POST", "/scene", `{"name":"off","extra":true}`, 400},
		{"POST", "/scene", `{"name":"off"} {}`, 400},
		{"POST", "/scene", strings.Repeat("x", 4097), 400},
		{"GET", "/scene", "", 405},
	} {
		recorder := httptest.NewRecorder()
		h.handler().ServeHTTP(recorder, httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body)))
		if recorder.Code != tt.code {
			t.Errorf("%s %s: got %d, want %d", tt.method, tt.path, recorder.Code, tt.code)
		}
	}
}

func TestUnixClientSceneAndStatus(t *testing.T) {
	h, f := singleHelper(t, quickTiming())
	path := socketPath(t)
	listener, err := listenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: h.handler(), ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		server.Close()
		<-done
	}()
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--socket", path, "scene", "off"}, &output); err != nil {
		t.Fatal(err)
	}
	var response reply
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Result["left"].Sent || response.Note != deliveryNote {
		t.Fatalf("unexpected scene response: %s", output.String())
	}
	f.take(t, 5)
	output.Reset()
	if err := run(context.Background(), []string{"--socket", path, "status"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"power":"off"`) {
		t.Fatalf("unexpected status: %s", output.String())
	}
	output.Reset()
	if err := run(context.Background(), []string{"--socket", path, "scene", "missing"}, &output); err == nil {
		t.Fatal("unknown scene did not fail client command")
	}
	if !strings.Contains(output.String(), "unknown scene") {
		t.Fatal("client discarded server error")
	}
	h.lights["left"].conn.Close()
	output.Reset()
	if err := run(context.Background(), []string{"--socket", path, "scene", "on"}, &output); err == nil {
		t.Fatal("UDP failure returned a successful client exit")
	}
	if !strings.Contains(output.String(), `"sent":false`) {
		t.Fatalf("missing per-light error: %s", output.String())
	}
}

func TestPartialSceneFailure(t *testing.T) {
	h := newHelper(context.Background(), testConfig(), quickTiming())
	defer h.close()
	f := attachFake(t, h.lights["left"])
	h.lights["right"].sourceIP = func(string) (string, error) {
		return "", os.ErrPermission
	}
	result, err := h.apply(context.Background(), "off")
	if err == nil || !result["left"].Sent || result["right"].Sent || result["right"].Error == "" {
		t.Fatalf("partial scene result not preserved: %+v, %v", result, err)
	}
	f.take(t, 5)
}

func TestRunRejectsBadArguments(t *testing.T) {
	for _, args := range [][]string{
		{}, {"unknown"}, {"scene"}, {"scene", "off", "extra"}, {"status", "extra"}, {"serve"},
	} {
		if err := run(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Errorf("accepted bad arguments %q", args)
		}
	}

}

func TestServeGracefulShutdown(t *testing.T) {
	path := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, path, testConfig()) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not create its socket")
		}
		time.Sleep(time.Millisecond)
	}
	var output bytes.Buffer
	if err := callHelper(context.Background(), path, "GET", "/status", nil, &output); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop gracefully")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket remains after shutdown: %v", err)
	}
}
