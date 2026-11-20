package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const deliveryNote = "UDP send only; delivery and physical light state are not acknowledged."

type reply struct {
	Note   string                 `json:"note"`
	Error  string                 `json:"error,omitempty"`
	Lights map[string]LightStatus `json:"lights,omitempty"`
	Result map[string]SceneResult `json:"result,omitempty"`
}

func writeReply(w http.ResponseWriter, code int, r reply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(r); err != nil {
		log.Printf("response write failed: %v", err)
	}
}

func (h *Helper) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeReply(w, http.StatusOK, reply{Note: deliveryNote, Lights: h.status()})
	})
	mux.HandleFunc("POST /scene", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Name string `json:"name"`
		}
		if err := readJSON(http.MaxBytesReader(w, r.Body, 4096), &request); err != nil {
			writeReply(w, http.StatusBadRequest, reply{Note: deliveryNote, Error: err.Error()})
			return
		}
		if _, ok := h.config.Scenes[request.Name]; !ok {
			writeReply(w, http.StatusNotFound, reply{Note: deliveryNote, Error: fmt.Sprintf("unknown scene %q", request.Name)})
			return
		}
		result, err := h.apply(r.Context(), request.Name)
		response := reply{Note: deliveryNote, Result: result}
		code := http.StatusOK
		if err != nil {
			response.Error = err.Error()
			code = http.StatusBadGateway
		}
		writeReply(w, code, response)
	})
	return mux
}

type localListener struct {
	*net.UnixListener
	lock *os.File
	once sync.Once
	err  error
}

func (l *localListener) Close() error {
	l.once.Do(func() {
		l.err = l.UnixListener.Close()
		if err := l.lock.Close(); l.err == nil {
			l.err = err
		}
	})
	return l.err
}

func listenLocal(path string) (*localListener, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("socket path must be absolute")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("socket parent %s must be a private directory (chmod 700), not a symlink", parent)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another helper holds the socket lock: %w", err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			lock.Close()
		}
	}()
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket file %s", path)
		}
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("a server is already listening at %s", path)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("cannot establish whether socket is stale: %w", err)
		}
		// The lifetime lock prevents two helpers from removing each other's socket.
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	keepLock = true
	return &localListener{UnixListener: listener, lock: lock}, nil
}

func serve(ctx context.Context, socket string, c Config) error {
	listener, err := listenLocal(socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	h := newHelper(ctx, c, defaultTiming)
	h.start()
	defer h.close()
	server := &http.Server{
		Handler:           h.handler(),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8192,
	}
	defer server.Close()
	done := make(chan error, 1)
	// HTTP shutdown closes the socket, but keep the singleton lock until light work stops.
	go func() { done <- server.Serve(listener.UnixListener) }()
	log.Printf("listening on %s; %s", socket, deliveryNote)
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	h.cancel()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		server.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func callHelper(ctx context.Context, socket, method, path string, body []byte, output io.Writer) error {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	request, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("helper request failed (is `nlite serve` running?): %w", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(output, response.Body); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("helper returned %s; see JSON response for per-light errors", response.Status)
	}
	return nil
}

func run(ctx context.Context, args []string, output io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("nlite", flag.ContinueOnError)
	socket := flags.String("socket", filepath.Join(home, ".neewer-gl1", "control.sock"), "absolute Unix socket path (before command)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: nlite [--socket PATH] serve --config FILE | scene NAME | status")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		flags.Usage()
		return errors.New("a command is required")
	}
	switch args[0] {
	case "serve":
		f := flag.NewFlagSet("serve", flag.ContinueOnError)
		config := f.String("config", "", "scene configuration JSON file (required)")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if *config == "" || f.NArg() != 0 {
			return errors.New("usage: nlite serve --config FILE")
		}
		c, err := loadConfig(*config)
		if err != nil {
			return err
		}
		return serve(ctx, *socket, c)
	case "scene":
		if len(args) != 2 {
			return errors.New("usage: nlite scene NAME")
		}
		body, err := json.Marshal(struct {
			Name string `json:"name"`
		}{args[1]})
		if err != nil {
			return err
		}
		return callHelper(ctx, *socket, http.MethodPost, "/scene", body, output)
	case "status":
		if len(args) != 1 {
			return errors.New("usage: nlite status")
		}
		return callHelper(ctx, *socket, http.MethodGet, "/status", nil, output)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
