# nlite

`nlite` is a Go background helper for controlling NEEWER GL1 Pro lights over
Wi-Fi. It builds on [Chippi/neewer-gl1-pro-cli](https://github.com/Chippi/neewer-gl1-pro-cli).

## Go background helper (macOS)

`nlite` builds a standalone binary using only the Go standard library. No
NEEWER Control Center, Stream Deck, or cloud account is needed to run it.

The helper keeps a separate UDP session for each light, sends heartbeats, and
accepts scene commands over a private Unix-domain socket. It does not expose a
TCP port or listen on your LAN. Lights within a scene run concurrently; entire
scene requests are serialized so their packets cannot interleave. Scene commands
wait until every light's UDP writes finish and exit nonzero on any reported
error, preserving per-light results if a scene only partially sends.

### Build and configure

Install Go 1.22 or newer, then from this repository:

```sh
go build -o bin/nlite ./cmd/nlite
install -d -m 700 "$HOME/.local/bin" "$HOME/.nlite"
install -m 755 bin/nlite "$HOME/.local/bin/nlite"
install -m 600 examples/scenes.json "$HOME/.nlite/scenes.json"
```

**Replace the example addresses before trying a scene.** `examples/scenes.json`
uses documentation-only addresses. Set the physical `left` and `right` light
IPs in your installed configuration. The configured scenes are:

| Scene | Left | Right |
|---|---|---|
| `video-calls` | On, 4300 K, 65% | On, 4300 K, 10% |
| `evening-ambient` | On, 2900 K, 5% | Off |
| `work` | On, 2900 K, 5% | On, 2900 K, 5% |
| `sleep` | Off | Off |

Use actual Kelvin values in the configuration (for example, `4300`).
On states require brightness 1-100 and temperature 2900-7000 K in
100 K steps. Off states contain only `"power": "off"`. Unknown JSON fields,
unknown lights, duplicate light IP addresses, and invalid ranges are rejected
before the helper starts. Light addresses must be IPv4 literals.

Scene names and light names are configurable. A scene may name just one light
to control it independently; omitted lights are left untouched. For example, an
additional scene entry can be `"left-off": {"left": {"power": "off"}}`.
Configuration is loaded on startup; restart the helper after editing it.

### Run and use from Shortcuts

Start the helper in a terminal (stop it with Control-C):

```sh
"$HOME/.local/bin/nlite" --socket "$HOME/.nlite/control.sock" serve --config "$HOME/.nlite/scenes.json"
```

Starting it alone sends **no packets to the lights**. In a second terminal:

```sh
"$HOME/.local/bin/nlite" --socket "$HOME/.nlite/control.sock" scene video-calls
"$HOME/.local/bin/nlite" --socket "$HOME/.nlite/control.sock" scene evening-ambient
"$HOME/.local/bin/nlite" --socket "$HOME/.nlite/control.sock" status
```

In macOS Shortcuts, create a shortcut with a **Run Shell Script** action and use
the absolute installed binary path, replacing `YOUR_USER`:

```sh
/Users/YOUR_USER/.local/bin/nlite --socket /Users/YOUR_USER/.nlite/control.sock scene video-calls
```

Use `scene evening-ambient` in the other shortcut. The helper must already be
running, either in the terminal or via launchd below. Neither Shortcut needs to
start the helper, initialize both lights itself, or run NEEWER software.

All commands print JSON. `"sent": true` means every datagram for that light was
accepted by the local OS, **not** that the light received it or changed state.
`status` reports the last requested settings, last successful scene-send time,
last packet-send time (including heartbeat/initialization), source IP, and last
transport error. These are not observations of the light's actual state.
After a failed request, `last_request` can be newer than `last_scene_sent_at`.
There is no reliable device-state acknowledgment in this implementation.

The examples use `$HOME/.nlite/control.sock`, with mode `0600` in a private
`0700` directory. Pass the same absolute socket path **before** the command on
both helper and clients:

```sh
nlite --socket /absolute/private/directory/control.sock serve --config /absolute/scenes.json
nlite --socket /absolute/private/directory/control.sock scene video-calls
```

For compatibility, omitting `--socket` still uses the original
`$HOME/.neewer-gl1/control.sock`; existing helpers and clients can keep using it.

Other processes running as your user can also issue commands. A lifetime file
lock prevents duplicate helpers, and a refused stale socket is recovered after
a crash. The adjacent `.lock` file normally remains on disk; do not delete it
while a helper is running. An occupied socket or an existing non-socket file is
never replaced.

### Start automatically with launchd

Use a per-user LaunchAgent, not a root/system daemon. Stop any foreground helper
first. If you have an older LaunchAgent installed, unload it before enabling this
one so two helpers don't control the same lights. Copy the example:

```sh
mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
cp examples/com.nlite.helper.plist "$HOME/Library/LaunchAgents/"
```

Edit the copied plist and replace **every** `/Users/YOUR_USER` with your actual
absolute home path. `launchd` does not expand `~`, `$HOME`, or shell expressions
in `ProgramArguments`. Keep the socket path identical to the one your clients
use. Then:

```sh
plutil -lint "$HOME/Library/LaunchAgents/com.nlite.helper.plist"
launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.nlite.helper.plist"
launchctl print "gui/$(id -u)/com.nlite.helper"
```

This starts the helper now, at future logins, and restarts it if it exits.
It does not run before you log in. Logs go to
`$HOME/Library/Logs/nlite.log`; rotate or remove old logs as needed.
To stop/unload it, or before reloading edited configuration:

```sh
launchctl bootout "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.nlite.helper.plist"
```

Run the `bootstrap` command again to reload. SIGTERM/Control-C cancels pending
work, stops heartbeats, closes UDP sockets, and removes the command socket.

### Timing, reconnection, and protocol limitations

The helper derives the source IPv4 address from the kernel's route **to each
light**, not from the first network interface. Initialization constructs the
handshake from the actual ASCII address length and additive checksum, sends it
three times, then sends `8006010188`. All writes to a given light, including
heartbeats, are serialized and spaced at least 500 ms apart.

A warm on/CCT scene typically needs 0.5-1 second of packet spacing, with the
lights running in parallel. A cold on/CCT scene needs at least 2.5 seconds
(cold off: 2 seconds). Go startup alone does not remove this protocol delay.
The helper sends `80040084` heartbeats about every 2 seconds after first use
and renews initialization about every minute. These intervals are conservative
protocol heuristics, not a guarantee of device acceptance.

Before each scene or heartbeat, it checks the route's source address and elapsed
wall time. A changed source, a gap of at least 6 seconds (including sleep), a
backwards clock jump, or session expiry triggers a fresh UDP socket and handshake.
An OS-reported route/write error closes that session and is exposed in JSON or
the helper log. The next scene attempts initialization again. Failed scenes are
**not** retried or replayed in the background, so old settings cannot overwrite
a newer scene. Partial failure does not roll back lights already sent commands.
Client requests time out after 30 seconds; cancellation can leave a partially
sent scene and cannot retract packets already sent.

Across routed VLANs, ping is not proof that UDP port 5052 and return traffic to
the client's ephemeral port are allowed. The helper does not change firewall
rules, use discovery broadcasts, or require both devices on the same subnet.
Use stable light IP addresses and the correct route. Wi-Fi/network changes and
device power cycles still require physical verification; a power-cycled light
can silently lose its session until the next periodic renewal.

**Hardware evidence:** the three dynamically checksummed handshakes followed by
`8006010188` and off packet `800502010088`, with 500 ms spacing, were physically
confirmed for one GL1 Pro across VLANs. Power-on and the Pro CCT packet
(`80050402` + brightness + temperature/100 + `32` + checksum) preserve this
repository's prior format but have not been physically confirmed in this work.
Heartbeat/session lifetime behavior is likewise unverified. Local fake-UDP tests
prove packet construction and scheduling, not radio delivery or light behavior.
After assigning left/right, observe each light while trying both scenes; if a
scene silently fails, issue the **current desired** scene again and inspect logs.
Do not treat a successful exit as confirmation of visible state.

### Minimal Linux container

The `nlite` image uses Alpine for a small runtime with `/bin/sh` and BusyBox
utilities. The Go binary is statically linked, and the container runs as the
non-root `nlite` user (UID/GID `65532`). Build for the ARM64 Pis:

```sh
docker buildx build --platform linux/arm64 --load -t nlite:local .
```

The GitHub Actions workflow publishes `linux/arm64` and `linux/amd64` images
to `ghcr.io/<owner>/nlite` on `v*` tags, using the repository's `GITHUB_TOKEN`.
GHCR package visibility is managed separately from repository visibility.

Mount your own scene configuration, readable by UID `65532`, at
`/config/scenes.json`. The image does not include the example configuration or
its light addresses. The default command starts the helper without exposing a
network service. Open a shell or invoke the binary directly:

```sh
kubectl exec -it <pod-name> -- /bin/sh
kubectl exec <pod-name> -- /nlite status
```

In the shell, `ip route` shows the pod's routes, and `/nlite scene video-calls`
sends the scene to the configured lights.

The helper needs writable storage at `$HOME` to create its private command
socket directory. If you use a read-only root filesystem, mount a volume
writable by UID `65532` at `/home/nlite`.

### Deploy to Kubernetes or k3s

The [Deployment](deploy/nlite.yaml) runs one replica without host networking or
a Service. It mounts `nlite-scenes` read-only at `/config` and uses an `emptyDir`
for the Unix socket. It starts no second replica during an update.

If you don't already have a local config, copy [the four-scene example](examples/scenes.json):

```sh
cp examples/scenes.json deploy/scenes.local.json
```

Replace the placeholder IPs in `deploy/scenes.local.json` with your light
addresses. The file is Git-ignored. Generate the ConfigMap from it, then apply
the Deployment in the same cluster and namespace:

```sh
kubectl create configmap nlite-scenes \
  --from-file=scenes.json=deploy/scenes.local.json \
  --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f deploy/nlite.yaml
kubectl rollout status deployment/nlite
kubectl exec deploy/nlite -- /nlite status
```

Once you're ready to observe the lights, run
`kubectl exec deploy/nlite -- /nlite scene video-calls`. You can substitute
`evening-ambient`, `work`, or `sleep` for `video-calls`.
If pod egress is masqueraded, the light may see a different source IP from the
one in the handshake. A successful send does not prove that the light accepted
the packet. After updating the ConfigMap, use `kubectl rollout restart deploy/nlite`
to reload it.

#### Trigger Kubernetes scenes from macOS Shortcuts

Use one **Run Shell Script** action per scene. Call `kubectl` directly rather
than relying on the terminal's shell aliases, environment, or active context:

```sh
/absolute/path/to/kubectl --kubeconfig "$HOME/.kube/homelab.yaml" \
  --context homelab exec deploy/nlite -- /nlite scene video-calls
```

Replace the binary path, kubeconfig, and context for your setup; add
`--namespace` before `exec` if needed. Use `evening-ambient`, `work`, or `sleep`
in additional shortcuts. This uses existing Kubernetes credentials and requires
access to the Kubernetes API, not a new port or token for `nlite`. For regular
use, prefer credentials scoped to this workload over a cluster-admin config.

One scene command already sends to its lights concurrently. Packets to the
same light stay ordered and spaced apart. An off action has one scene packet;
an on action sends power-on and then brightness/temperature. Independent
session renewals and device processing can also make visible changes staggered.
`status` reports each light's `last_scene_sent_at` for comparing send completion,
not the time the light visibly changed.

### Go development checks

```sh
go test -race ./...
go vet ./...
go build -o bin/nlite ./cmd/nlite
```

Tests use local fake UDP servers and temporary Unix sockets only; they never
contact the example light IPs or install/start a persistent service.

## Acknowledgments
Special thanks to [braintapper](https://github.com/braintapper/neewer-gl1) for the reverse engineering efforts done for Neewer GL1.
