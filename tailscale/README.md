# Embedded Tailscale engine (gomobile AAR source)

This directory builds `tailscale.aar` — a gomobile binding of a userspace
`tailscaled` embedded in the Rethink app process. The AAR is consumed by
`app/build.gradle` (see "Embedded-Tailscale AAR" there).

## How it works

`engine.go` follows the same recipe as `tailscaled
--tun=userspace-networking --socks5-server=127.0.0.1:1055`, but as a library:

- `wgengine.NewUserspaceEngine` with a fake TUN (no root, no VpnService);
  `sys.Tun.Get().Start()` is called explicitly — without it the wrapper's
  goroutines never run and every tailnet dial times out while control-plane
  traffic looks healthy
- `netstack.Create` + `NetstackRouter.Set(true)` so the dialer egresses via
  the tailnet (including exit-node-routed destinations)
- plain `socks5.Server{Dialer: dialer.UserDial}` on loopback port 1055
  (Rethink dials `socks5://` only; no HTTP CONNECT multiplexing)
- a plain-DNS relay on loopback port 1053 that feeds raw packets to
  `dns.Manager.Query` (the same entry point netstack uses for TUN-intercepted
  queries) so MagicDNS names resolve for the app's split-DNS mode; dialing
  `100.100.100.100:53` through netstack does not work — that address is
  intercepted at the fake-TUN layer before gVisor sees it
- `ipnlocal.LocalBackend` for login/state/exit-node control, notifications
  pushed to Kotlin through the `Callback` interface; a changed coordination
  URL (Headscale) is force-applied after start via `EditPrefs`
- Android embedding shims: network interfaces come from Java via
  `Callback.GetInterfacesAsJson()` (Go's `net.Interfaces` is blocked on
  SDK 30+), `paths.AppSharedDir` / `TS_LOGS_DIR` are set so logpolicy finds a
  writable dir, Taildrop is compiled out (`ts_omit_taildrop`), and diagnostics
  tee into `logs/engine.log` inside the state dir

Rethink's firestack tunnel then adds `socks5://127.0.0.1:1055` to its proxy
chain; per-app routing works exactly like any other Rethink proxy.

## Building the AAR

The output is a **combined** AAR: firestack's exported packages (`intra`,
`intra/backend`, `intra/settings`) bound together with this wrapper in a
single `libgojni.so`. gomobile hardcodes `libgojni.so` and `go.Seq`, so two
separate AARs cannot coexist in one process — and an AAR bound from this
directory alone lacks the `intra.*` bindings the app compiles against.

Requirements: Go >= 1.26, Android SDK + NDK (CI pins `28.2.13676358`), JDK 17.
The authoritative, CI-identical recipe is
`.github/actions/build-tailscale-aar/action.yml`; the short local version:

```sh
cd tailscale
# gomobile/gobind come from this module's own pinned x/mobile (tool
# directives); do not install @latest or bind and gobind drift apart
go mod tidy
go install golang.org/x/mobile/cmd/gobind golang.org/x/mobile/cmd/gomobile
gomobile init

# runtime overlay: firestack's write_err patch applied by hand (see action.yml)
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

CI does this automatically (`.github/workflows/tailscale-aar.yml`, and via
the composite action before every APK build) and publishes `tailscale.aar`
on GitHub release tags `ts-aar-v*`.

## Consuming the AAR

- default (`-PtailscaleRepo=release`): downloaded from GitHub releases
  (`ts-aar-v<version>` tags; asset name `tailscale.aar`)
- local build: `-PtailscaleRepo=local -PtailscaleAarPath=tailscale/tailscale.aar`
- or drop the file at `app/libs/tailscale.aar` (takes precedence over both)

## Kotlin side

`com.celzero.bravedns.service.TailscaleManager` owns the engine lifecycle;
settings live under Proxy → Tailscale. The tunnel consumes the engine as
proxy id `TS` (`socks5://127.0.0.1:1055`) plus a separate `TSDNS` DNS
transport pointing at `127.0.0.1:1053`; per-app routing works exactly like
any other Rethink proxy (see `docs/tailscale.md`).
