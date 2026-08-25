# Embedded Tailscale integration

This fork embeds a userspace **tailscaled** inside Rethink so apps can reach a
tailnet (and exit nodes) without surrendering Rethink's VpnService slot.

## Architecture

```
┌──────────────────────────────── Android app process ────────────────────────────┐
│                                                                                 │
│  Apps' traffic ─► tun ─► firestack tunnel ──► proxies                           │
│                              │                 ▲                                 │
│                              │ per-app flow    │ socks5://127.0.0.1:1055         │
│                              ▼                 │ ("TS" proxy id)                 │
│                    TunFlowManager ─────────────┘                                 │
│                    (returns "TS" for opted-in apps,                             │
│                     after WireGuard, before global                              │
│                     Orbot/SOCKS5/HTTP proxies)                                  │
│                                                                                 │
│  TunDnsManager ── *.tailnet-suffix queries ──► "TSDNS" transport                │
│        (split-DNS mode)                       │                                 │
│                                               ▼                                 │
│  TailscaleManager ◄── state/login events ── Engine (combined gomobile AAR)      │
│         │                                              │                        │
│         │ Start/Stop/Login/SetExitNode/SetCorpDNS      ▼                        │
│         │                                    tsd.System (userspace)               │
│         │                                    ├ wgengine (fake tun, started)      │
│         │                                    ├ netstack                          │
│         │                                    ├ tsdial.Dialer ── tailnet          │
│         │                                    ├ socks5.Server :1055               │
│         │                                    └ dns relay :1053 ── dns.Manager    │
│  PersistentState (prefs)                store/ (node identity, logs/)           │
└─────────────────────────────────────────────────────────────────────────────────┘
```

Key decisions:

1. **Userspace networking only** (`--tun=userspace-networking` equivalent).
   Rethink owns Android's single VpnService; the embedded node never creates a
   TUN. The exact recipe is the one `tailscaled` uses for its own SOCKS5 flag:
   fake-TUN userspace engine + netstack + `socks5.Server{Dialer:
   dialer.UserDial}` served on loopback. The fake-TUN wrapper's goroutines are
   started explicitly (`sys.Tun.Get().Start()`); without that call every
   tailnet dial stalls with context-deadline errors while the control plane
   looks healthy.
2. **Proxy-chain reuse.** The tailnet is just another Rethink proxy with id
   `TS`. Per-app routing reuses the existing `ProxyApplicationMapping`
   machinery (`WgIncludeAppsDialog`) and the flow hook in `TunFlowManager`
   sits between WireGuard and the global Orbot/SOCKS5/HTTP proxies, so
   app-specific WireGuard rules still win.
3. **No socket-protect needed.** By default Rethink excludes its own package
   from the VPN builder (`routeRethinkInRethink=false`), so the engine's
   WireGuard/DERP/control sockets egress directly. Enabling "route rethink in
   rethink" would loop them; the engine refuses to start while that setting is
   on, and the enable-switch warns about it.
4. **DNS is split, not surrendered.** Only queries whose name matches the
   tailnet's MagicDNS suffix (e.g. `host.tailnet-name.ts.net.`) are routed to
   the engine's resolver, registered under transport id `TSDNS`; everything
   else keeps flowing through Rethink's regular DoH/DoT/DNSCrypt pipeline.
   Two hard-won constraints live here:
   - the resolver must be registered via `Intra.addDNSProxy` with an id
     **different** from the proxy id (`TSDNS` vs `TS`) — firestack attaches a
     dns53 transport's relay to the proxy sharing its id, which would push
     loopback queries through the CONNECT-only socks5 endpoint where they
     time out;
   - the relay feeds raw packets to `dns.Manager.Query` (the netstack entry
     point) instead of dialing `100.100.100.100:53`, which is intercepted at
     the fake-TUN layer before gVisor ever sees it and blackholes.
   Exit-node traffic works fully regardless of MagicDNS.

## Embedding gotchas (all fixed in engine.go, listed here so they stay fixed)

- Go's `net.Interfaces` is blocked for apps on SDK 30+, so `netmon` sources
  its initial snapshot from Kotlin via `Callback.GetInterfacesAsJson()`
  (registered with `netmon.RegisterInterfaceGetter` before `netmon.New`;
  schema mirrors libtailscale's ifaceparse).
- `logpolicy.LogsDir` panics with *"no safe place found to store log state"*
  unless `paths.AppSharedDir` points at app-private storage *and*
  `TS_LOGS_DIR` / cache env vars are set before backend construction.
- Taildrop is compiled out (`ts_omit_taildrop`): its extension has no file
  ops wired in this embedding and nil-panics on profile changes. SOCKS5-only
  routing needs no file transfer.
- A changed coordination-server URL (Headscale) is enforced after
  `lb.Start()` via `EditPrefs{ControlURLSet:true}`; `ControlURL` is otherwise
  only read at backend start, so a stale stored profile would keep dialing
  Tailscale SaaS.
- Engine diagnostics tee into `files/tailscale/logs/engine.log` (2 MiB,
  rotate-on-start) and are attached to bug reports as
  `tailscale_engine_log.txt`.

## File map

| Path | Purpose |
| --- | --- |
| `tailscale/engine.go` | gomobile-bound engine (userspace tailscaled + SOCKS5 + DNS relay) |
| `tailscale/engine_test.go` | Go unit tests (log-id persistence) |
| `tailscale/go.mod`, `tailscale/go.sum` | pins `tailscale.com v1.102.3` and `firestack@61894b7fdb`; declares gomobile/gobind as module tools |
| `tailscale/tools/runtime_write_err_android.patch` | vendored firestack runtime-overlay patch applied during the bind |
| `.github/workflows/tailscale-aar.yml` | builds/publishes `tailscale.aar` releases (`ts-aar-v*` tags) |
| `.github/actions/build-tailscale-aar/` | composite action used by all APK workflows |
| `service/TailscaleManager.kt` | engine lifecycle, state LiveData, prefs bridge, RINR guard |
| `net/go/GoVpnAdapter.kt` | adds/removes the `TS` socks5 proxy + `TSDNS` resolver to/from the tunnel |
| `service/TunFlowManager.kt` | per-flow routing decision into `TS` |
| `service/GlobalProxyHandler.kt` | re-adds `TS` if it disappears from the tunnel |
| `tunnel/TunDnsManager.kt` | routes MagicDNS-suffix queries to the `TSDNS` transport |
| `ui/activity/TailscaleSettingsActivity.kt` (main source set) | enable / login (browser or auth key) / Headscale URL / exit node / advertise & accept routes / shields-up / MagicDNS / app assignment |
| `res/layout/activity_tailscale_settings.xml` | settings screen layout (on the `main` res path via `src/full/res`) |

## Build & consume the AAR

One AAR must carry **both** firestack and the Tailscale wrapper: gomobile
hardcodes `libgojni.so` and the `go.Seq` JNI bridge, so two separate AARs
cannot coexist in one process. Binding only `./tailscale` produces an AAR
that lacks the `intra.*` bindings the app compiles against and will fail the
Kotlin build.

The authoritative recipe lives in
`.github/actions/build-tailscale-aar/action.yml`. Locally, the short version:

```sh
cd tailscale
go mod tidy && go install golang.org/x/mobile/cmd/gobind golang.org/x/mobile/cmd/gomobile
gomobile init

# runtime overlay (see action.yml for the hand-applied patch step)
mkdir -p build/src/runtime
cp "$(go env GOROOT)/src/runtime/write_err_android.go" build/src/runtime/
(cd build && patch -p1 -f < ../tools/runtime_write_err_android.patch)
printf '{"Replace":{"src/runtime/write_err_android.go":"%s"}}' \
  "$PWD/build/src/runtime/write_err_android.go" > build/overlay.json

export CGO_LDFLAGS='-Wl,-z,max-page-size=16384'
gomobile bind -trimpath -a \
  -javapkg=com.celzero.firestack \
  -androidapi 23 -target=android -tags='android,ts_omit_taildrop' \
  -overlay="$(pwd)/build/overlay.json" \
  -ldflags '-checklinkname=0 -w -s -buildid=' \
  -gcflags='-trimpath' \
  -o tailscale.aar \
  github.com/celzero/firestack/intra \
  github.com/celzero/firestack/intra/backend \
  github.com/celzero/firestack/intra/settings \
  .
```

Then either copy the AAR to `app/libs/tailscale.aar` or point the build at
it:

```sh
./gradlew -PtailscaleRepo=local -PtailscaleAarPath=tailscale/tailscale.aar assembleFdroidFullDebug
```

CI builds the AAR automatically (composite action) before every APK build;
release assets come from the `ts-aar-v*` tags via `-PtailscaleRepo=release`
(the default resolution mode).

## Testing

- Kotlin: `app/src/test/.../TailscaleManagerStateTest.kt` (state mapping).
- Go: `cd tailscale && go test ./...`.
- Manual: enable Tailscale → log in (SaaS or Headscale URL) → assign an app →
  verify the app reaches a tailnet IP / exit-node-routed destination; toggle
  off and confirm the proxy disappears from the tunnel. With MagicDNS on,
  resolve `<peer>.<tailnet-suffix>` from a mapped app and confirm the query
  lands on the `TSDNS` transport (console logs tag it `(onQuery)magic dns
  suffix match`), while ordinary domains still resolve via the configured
  Rethink resolver.
