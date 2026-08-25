// Package tailscale embeds a userspace tailscaled inside Rethink DNS +
// Firewall.
//
// The engine runs Tailscale entirely in userspace networking mode (netstack):
// no TUN device is created and no Android VpnService is claimed (Rethink
// already owns the device's single VPN slot). Instead a SOCKS5 (+ HTTP
// CONNECT) listener is served on 127.0.0.1:1055 whose dialer routes through
// the tailnet; Rethink's firestack tunnel adds that endpoint to its proxy
// chain so tailnet / exit-node traffic flows through Rethink's existing
// per-app proxy plumbing.
//
// This mirrors the recipe tailscaled itself uses for
// --tun=userspace-networking + --socks5-server; see
// cmd/tailscaled/{tailscaled,proxy,netstack}.go in github.com/tailscale/tailscale.
//
// Bound to Java/Kotlin via gomobile as com.celzero.bravedns.tailscale.*.
package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "tailscale.com/feature/condregister" // register optional features

	// blank-imported so `go mod tidy` keeps firestack in the module graph;
	// this module is bound together with firestack into ONE libgojni.so (two
	// separate gomobile AARs cannot coexist: each hardcodes libgojni.so and
	// the go.Seq JNI bridge). See .github/actions/build-tailscale-aar.
	_ "github.com/celzero/firestack/intra/settings"

	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnauth"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/store"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/net/dns"
	"tailscale.com/net/netmon"
	"tailscale.com/net/socks5"
	"tailscale.com/net/tsdial"
	"tailscale.com/paths"
	"tailscale.com/tailcfg"
	"tailscale.com/tsd"
	"tailscale.com/types/logger"
	"tailscale.com/types/logid"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/netstack"
)

// ipn.State values mirrored for Kotlin (see TailscaleManager.TsState).
const (
	StateNoState          = int(ipn.NoState)
	StateInUseOtherUser   = int(ipn.InUseOtherUser)
	StateNeedsLogin       = int(ipn.NeedsLogin)
	StateNeedsMachineAuth = int(ipn.NeedsMachineAuth)
	StateStopped          = int(ipn.Stopped)
	StateStarting         = int(ipn.Starting)
	StateRunning          = int(ipn.Running)
)

// Socks5Addr is the loopback endpoint serving SOCKS5.
const Socks5Addr = "127.0.0.1:1055"

// DNSAddr is the loopback endpoint serving the engine's resolver (MagicDNS)
// as plain DNS53; registered with firestack via Intra.AddDNSProxy so that
// tailnet-name queries can be routed to transport id "TSDNS".
const DNSAddr = "127.0.0.1:1053"

// AutoAnyExitNode selects any available exit node.
const AutoAnyExitNode = "auto:any"

// Callback receives asynchronous events from the engine. Implemented in
// Kotlin; methods are invoked on arbitrary Go threads.
type Callback interface {
	// OnStateChange reports ipn.State transitions (values above).
	OnStateChange(state int)
	// OnBrowseToURL delivers an interactive login URL that must be opened in
	// a browser (or WebView) to complete authentication.
	OnBrowseToURL(url string)
	// OnError reports recoverable errors (health warnings, dial failures).
	OnError(message string)
	// GetInterfacesAsJson returns a JSON array describing the device's
	// network interfaces, since Go's net.Interfaces is blocked for apps on
	// Android SDK 30+. Schema (same as tailscale-android):
	//   [{"name","index","mtu","up","broadcast","loopback",
	//     "pointToPoint","multicast","addrs":[{"ip","prefixLen"}]}]
	GetInterfacesAsJson() string
}

// Engine owns one embedded tailscaled instance. Create with NewEngine,
// boot with Start, tear down with Stop (an Engine cannot be restarted;
// create a fresh one instead).
type Engine struct {
	mu sync.Mutex
	cb Callback

	sys    *tsd.System
	lb     *ipnlocal.LocalBackend
	ns     *netstack.Impl
	dialer *tsdial.Dialer
	dns    *dns.Manager

	socksLn net.Listener
	cancel  context.CancelFunc

	dnsUdp net.PacketConn
	dnsTcp net.Listener

	started bool
	stopped bool
}

// NewEngine constructs an Engine. stateDir persists node identity across app
// restarts (pass "" for memory-only state). cb may be nil.
func NewEngine(stateDir string, cb Callback) (*Engine, error) {
	logf := logger.Logf(log.Printf)

	// Android app processes have no writable default temp/cache dirs, which
	// makes logpolicy.LogsDir panic ("no safe place found to store log
	// state") inside NewLocalBackend. Point it at our state dir and set up
	// the env vars os.UserCacheDir/os.MkdirTemp rely on, same as
	// tailscale-android's start().
	if stateDir != "" {
		paths.AppSharedDir.Store(stateDir)
		logDir := filepath.Join(stateDir, "logs")
		if err := os.MkdirAll(logDir, 0o700); err == nil {
			os.Setenv("TS_LOGS_DIR", logDir)
		}
		if _, exists := os.LookupEnv("XDG_CACHE_HOME"); !exists {
			os.Setenv("XDG_CACHE_HOME", filepath.Join(stateDir, "cache"))
		}
		if _, exists := os.LookupEnv("HOME"); !exists {
			os.Setenv("HOME", stateDir)
		}
		setupPersistentLog(filepath.Join(logDir, "engine.log"))
	}

	sys := tsd.NewSystem()

	if stateDir != "" {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("state dir: %w", err)
		}
		st, err := store.New(logf, filepath.Join(stateDir, "tailscaled.state"))
		if err != nil {
			return nil, fmt.Errorf("state store: %w", err)
		}
		sys.Set(st)
	} else {
		st, err := mem.New(logf, "")
		if err != nil {
			return nil, fmt.Errorf("memory store: %w", err)
		}
		sys.Set(st)
	}

	// Android (SDK 30+) blocks Go's net.Interfaces, so netmon must source
	// the interface list from the app (java.net.NetworkInterface) — same as
	// tailscale-android. Register before netmon.New so its initial snapshot
	// uses it.
	if cb != nil {
		netmon.RegisterInterfaceGetter(func() ([]netmon.Interface, error) {
			return parseInterfacesJSON(cb.GetInterfacesAsJson())
		})
	}

	netMon, err := netmon.New(sys.Bus.Get(), logf)
	if err != nil {
		return nil, fmt.Errorf("netmon: %w", err)
	}
	sys.Set(netMon)

	dialer := &tsdial.Dialer{Logf: logf}
	dialer.SetBus(sys.Bus.Get())
	sys.Set(dialer)

	// userspace engine with fake tun/router/dns configurators: pure netstack
	// mode, the equivalent of tailscaled --tun=userspace-networking
	conf := wgengine.Config{
		NetMon:        netMon,
		HealthTracker: sys.HealthTracker.Get(),
		Metrics:       sys.UserMetricsRegistry(),
		Dialer:        dialer,
		SetSubsystem:  sys.Set,
		EventBus:      sys.Bus.Get(),
	}
	eng, err := wgengine.NewUserspaceEngine(logf, conf)
	if err != nil {
		return nil, fmt.Errorf("userspace engine: %w", err)
	}
	sys.Set(eng)
	sys.NetstackRouter.Set(true)

	// Mandatory: start the tun wrapper's internal goroutines. Without this
	// every WG packet queues behind "tstun: awaiting Wrapper.Start call" and
	// all data-path dials time out (control plane still works, which masks it).
	if w, ok := sys.Tun.GetOK(); ok {
		w.Start()
	}

	ns, err := netstack.Create(logf, sys.Tun.Get(), eng, sys.MagicSock.Get(),
		dialer, sys.DNSManager.Get(), sys.ProxyMapper())
	if err != nil {
		return nil, fmt.Errorf("netstack: %w", err)
	}
	ns.ProcessLocalIPs = true
	ns.ProcessSubnets = true
	sys.Set(ns)

	logID, err := loadOrCreateLogID(stateDir)
	if err != nil {
		logID = deadLogID()
	}
	lb, err := ipnlocal.NewLocalBackend(logf, logID.Public(), sys, 0)
	if err != nil {
		return nil, fmt.Errorf("local backend: %w", err)
	}

	hostinfo.SetApp("rethink-dns")
	return &Engine{cb: cb, sys: sys, lb: lb, ns: ns, dialer: dialer, dns: sys.DNSManager.Get()}, nil
}

// Start boots the backend, routes the dialer through netstack, and serves the
// loopback SOCKS5 server.
//
// hostname may be empty (the coordination server assigns one). controlURL may
// be empty (Tailscale SaaS) or point at a Headscale-compatible server.
// authKey, if non-empty, authenticates non-interactively; otherwise the
// engine lands in NeedsLogin and LoginInteractive triggers the URL flow.
func (e *Engine) Start(hostname, controlURL, authKey string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return fmt.Errorf("engine already started")
	}
	if e.stopped {
		return fmt.Errorf("engine already stopped; create a new engine")
	}

	ctx, cancel := context.WithCancel(context.Background())

	prefs := ipn.NewPrefs()
	prefs.WantRunning = true
	prefs.ControlURL = controlURL
	prefs.Hostname = hostname

	opts := ipn.Options{
		UpdatePrefs: prefs,
		AuthKey:     authKey,
	}
	log.Printf("ts-engine: starting backend (control=%q, hostname=%q, authkey=%v)",
		controlURL, hostname, authKey != "")
	if err := e.lb.Start(opts); err != nil {
		cancel()
		return fmt.Errorf("backend start: %w", err)
	}

	// Belt-and-suspenders: enforce ControlURL after start as well. If a
	// stale/default profile existed, lb.Start may have spun up a control
	// client toward controlplane.tailscale.com; EditPrefs forces the
	// reconfiguration to the intended coordination server.
	if controlURL != "" {
		p := &ipn.MaskedPrefs{}
		p.ControlURLSet = true
		p.Prefs.ControlURL = controlURL
		if _, err := e.lb.EditPrefs(p); err != nil {
			e.reportErr("enforce control URL: %v", err)
		}
	}

	// When an auth key is supplied, force a login even if a persisted node is
	// currently logged out. LocalBackend.Start only auto-logs-in via cc.Login
	// inside startLocked when the node is NOT logged out; a logged-out node
	// therefore ignores the key and stalls at NeedsLogin. StartLoginInteractive
	// calls cc.Login directly, bypassing that loggedOut guard, so the key is
	// consumed and the node re-authorizes.
	if authKey != "" {
		if err := e.lb.StartLoginInteractive(context.Background()); err != nil {
			e.reportErr("auth-key login: %v", err)
		}
	}

	// route the dialer through netstack (as tailscaled does in onlyNetstack
	// mode); PeerForIP covers both tailnet IPs and exit-node-routed
	// destinations (default route via 0.0.0.0/0).
	e.dialer.UseNetstackForIP = func(ip netip.Addr) bool {
		_, ok := e.lb.PeerForIP(ip)
		return ok
	}
	userDialWithLog := func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := e.dialer.UserDial(ctx, network, addr)
		if err != nil {
			log.Printf("ts-socks5: UserDial %s %s failed: %v", network, addr, err)
		} else {
			log.Printf("ts-socks5: UserDial %s %s ok", network, addr)
		}
		return conn, err
	}
	e.dialer.NetstackDialTCP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		c, err := e.ns.DialContextTCP(ctx, dst)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	e.dialer.NetstackDialUDP = func(ctx context.Context, dst netip.AddrPort) (net.Conn, error) {
		c, err := e.ns.DialContextUDP(ctx, dst)
		if err != nil {
			return nil, err
		}
		return c, nil
	}

	if err := e.ns.Start(e.lb); err != nil {
		cancel()
		return fmt.Errorf("netstack start: %w", err)
	}

	// loopback SOCKS5 server, as tailscaled --socks5-server; Rethink's
	// proxy chain dials it via the socks5:// scheme
	ln, err := net.Listen("tcp", Socks5Addr)
	if err != nil {
		cancel()
		return fmt.Errorf("socks5 listen: %w", err)
	}
	e.socksLn = ln
	e.cancel = cancel

	socksSrv := &socks5.Server{
		Logf:   logger.WithPrefix(log.Printf, "ts-socks5: "),
		Dialer: userDialWithLog,
	}
	go func() {
		if err := socksSrv.Serve(ln); err != nil {
			e.reportErr("socks5 server exited: %v", err)
		}
	}()

	go e.watchNotifications(ctx)

	// loopback DNS relay; best-effort: without it MagicDNS-suffixed queries
	// have no firestack transport to land on, but the socks5 data path is
	// unaffected
	if err := e.startDNSRelay(ctx); err != nil {
		e.reportErr("dns relay: %v", err)
	}

	e.started = true
	log.Printf("ts-engine: started; socks5 on %s, dns on %s", Socks5Addr, DNSAddr)
	return nil
}

// LoginInteractive begins the browser-based login flow; the URL is delivered
// via Callback.OnBrowseToURL.
func (e *Engine) LoginInteractive() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return fmt.Errorf("engine not started")
	}
	return e.lb.StartLoginInteractive(context.Background())
}

// Logout forgets the current node identity and returns to NeedsLogin.
func (e *Engine) Logout() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return fmt.Errorf("engine not started")
	}
	return e.lb.Logout(context.Background(), ipnauth.Self)
}

// Stop shuts the engine down. The instance cannot be reused afterwards.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.stopped {
		return
	}
	p := &ipn.MaskedPrefs{}
	p.WantRunningSet = true
	p.Prefs.WantRunning = false
	if _, err := e.lb.EditPrefs(p); err != nil {
		e.reportErr("stop: %v", err)
	}
	if e.socksLn != nil {
		e.socksLn.Close()
		e.socksLn = nil
	}
	if e.dnsUdp != nil {
		e.dnsUdp.Close()
		e.dnsUdp = nil
	}
	if e.dnsTcp != nil {
		e.dnsTcp.Close()
		e.dnsTcp = nil
	}
	if e.cancel != nil {
		e.cancel()
	}
	e.lb.Shutdown()
	e.started = false
	e.stopped = true
	log.Printf("ts-engine: stopped")
}

// SetExitNode routes proxied traffic via an exit node. idOrIP may be:
//
//	""          disable exit-node routing
//	"auto:any"  use any available exit node
//	node ID     stable ID of an exit node (e.g. from StatusJSON peers)
//	IP          tailnet IP of an exit node
func (e *Engine) SetExitNode(idOrIP string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return fmt.Errorf("engine not started")
	}
	// resolve node names (e.g. "alma") to stable IDs via the peers list
	resolved := e.resolveExitNodeLocked(idOrIP)
	p := &ipn.MaskedPrefs{}
	switch {
	case idOrIP == "":
		p.ExitNodeIDSet = true
		p.Prefs.ExitNodeID = ""
	case strings.EqualFold(idOrIP, AutoAnyExitNode):
		p.ExitNodeIDSet = true
		p.Prefs.ExitNodeID = tailcfg.StableNodeID(AutoAnyExitNode)
	default:
		if addr, err := netip.ParseAddr(resolved); err == nil {
			p.ExitNodeIPSet = true
			p.Prefs.ExitNodeIP = addr
			p.ExitNodeIDSet = true
			p.Prefs.ExitNodeID = ""
		} else {
			p.ExitNodeIDSet = true
			p.Prefs.ExitNodeID = tailcfg.StableNodeID(resolved)
		}
	}
	if _, err := e.lb.EditPrefs(p); err != nil {
		return fmt.Errorf("set exit node: %w", err)
	}
	return nil
}

// resolveExitNodeLocked resolves a node name to its StableID using the peers
// list. Must be called with e.mu held. Returns the input unchanged if no match.
func (e *Engine) resolveExitNodeLocked(nameOrIP string) string {
	if _, err := netip.ParseAddr(nameOrIP); err == nil {
		return nameOrIP
	}
	st := e.lb.Status()
	if st == nil {
		return nameOrIP
	}
	lower := strings.ToLower(nameOrIP)
	for _, p := range st.Peer {
		if p == nil {
			continue
		}
		if strings.ToLower(p.HostName) == lower {
			return string(p.ID)
		}
		dnsBase := strings.TrimSuffix(p.DNSName, ".")
		if strings.ToLower(dnsBase) == lower || strings.HasPrefix(strings.ToLower(p.DNSName), lower) {
			return string(p.ID)
		}
	}
	return nameOrIP
}

// SetAdvertiseRoutes sets the comma-separated CIDRs this node advertises as
// subnet routes (e.g. "10.0.0.0/24,192.168.1.0/24"; "" clears them all).
func (e *Engine) SetAdvertiseRoutes(cidrs string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return fmt.Errorf("engine not started")
	}
	var routes []netip.Prefix
	for _, c := range strings.Split(cidrs, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		pfx, err := netip.ParsePrefix(c)
		if err != nil {
			return fmt.Errorf("bad cidr %q: %w", c, err)
		}
		routes = append(routes, pfx)
	}
	p := &ipn.MaskedPrefs{}
	p.AdvertiseRoutesSet = true
	p.Prefs.AdvertiseRoutes = routes
	if _, err := e.lb.EditPrefs(p); err != nil {
		return fmt.Errorf("advertise routes: %w", err)
	}
	return nil
}

// SetShieldsUp toggles shields-up mode (drop inbound connections from the
// tailnet).
func (e *Engine) SetShieldsUp(up bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return fmt.Errorf("engine not started")
	}
	p := &ipn.MaskedPrefs{}
	p.ShieldsUpSet = true
	p.Prefs.ShieldsUp = up
	if _, err := e.lb.EditPrefs(p); err != nil {
		return fmt.Errorf("shields up: %w", err)
	}
	return nil
}

// SetAcceptRoutes toggles whether the node accepts subnet routes advertised
// by other nodes on the tailnet.
func (e *Engine) SetAcceptRoutes(accept bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return fmt.Errorf("engine not started")
	}
	p := &ipn.MaskedPrefs{}
	p.RouteAllSet = true
	p.Prefs.RouteAll = accept
	if _, err := e.lb.EditPrefs(p); err != nil {
		return fmt.Errorf("accept routes: %w", err)
	}
	return nil
}

// SetCorpDNS toggles MagicDNS (Tailscale's network DNS). When enabled, the
// node uses the coordination server's DNS resolver for tailnet domains.
func (e *Engine) SetCorpDNS(enabled bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return fmt.Errorf("engine not started")
	}
	p := &ipn.MaskedPrefs{}
	p.CorpDNSSet = true
	p.Prefs.CorpDNS = enabled
	if _, err := e.lb.EditPrefs(p); err != nil {
		return fmt.Errorf("corp dns: %w", err)
	}
	return nil
}

// MagicDNSSuffix returns the tailnet's MagicDNS suffix (e.g. ".tail1234.ts.net."
// or ".alma.n.") from the current status. Returns "" if not connected.
func (e *Engine) MagicDNSSuffix() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.lb == nil {
		return ""
	}
	st := e.lb.Status()
	if st != nil && st.CurrentTailnet != nil {
		return st.CurrentTailnet.MagicDNSSuffix
	}
	return st.MagicDNSSuffix
}

// CurrentState returns the current ipn.State as an int (maps to TsState enum
// in Kotlin). Useful for querying the state after Start() returns instead of
// relying on the asynchronous watchNotifications goroutine.
func (e *Engine) CurrentState() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.lb == nil {
		return 0 // ipn.NoState
	}
	st := e.lb.Status()
	if st == nil {
		return 0
	}
	switch st.BackendState {
	case "Running":
		return 6 // ipn.Running
	case "Starting":
		return 5 // ipn.Starting
	case "NeedsLogin":
		return 2 // ipn.NeedsLogin
	case "NeedsMachineAuth":
		return 3 // ipn.NeedsMachineAuth
	case "Stopped":
		return 4 // ipn.Stopped
	case "InUseOtherUser":
		return 1 // ipn.InUseOtherUser
	default:
		return 0
	}
}

// NodeInfo returns a JSON string with the current node's name and IPv4 address.
// Example: {"name":"my-phone","ipv4":"100.64.0.1"}. Returns "{}" if not connected.
func (e *Engine) NodeInfo() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.lb == nil {
		return "{}"
	}
	st := e.lb.Status()
	if st == nil || st.Self == nil {
		return "{}"
	}
	self := st.Self
	info := struct {
		Name string `json:"name"`
		IPv4 string `json:"ipv4"`
		IPv6 string `json:"ipv6"`
	}{
		Name: self.HostName,
	}
	if len(self.TailscaleIPs) > 0 {
		info.IPv4 = self.TailscaleIPs[0].String()
	}
	if len(self.TailscaleIPs) > 1 {
		info.IPv6 = self.TailscaleIPs[1].String()
	}
	b, err := json.Marshal(info)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ResolveExitNode resolves a node name or IP to a node ID suitable for
// SetExitNode. It searches the peers list for a matching hostname, DNS name,
// or IPv4/IPv6 address. Returns the resolved node ID, or the input unchanged
// if no peer match is found (allows SetExitNode to handle it as-is).
func (e *Engine) ResolveExitNode(nameOrIP string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.lb == nil {
		return nameOrIP
	}
	st := e.lb.Status()
	if st == nil {
		return nameOrIP
	}
	// if it's already an IP, return as-is
	if _, err := netip.ParseAddr(nameOrIP); err == nil {
		return nameOrIP
	}
	lower := strings.ToLower(nameOrIP)
	for _, p := range st.Peer {
		if p == nil {
			continue
		}
		// match hostname
		if strings.ToLower(p.HostName) == lower {
			return string(p.ID)
		}
		// match DNSName (e.g. "alma.alma.n.")
		dnsBase := strings.TrimSuffix(p.DNSName, ".")
		if strings.ToLower(dnsBase) == lower || strings.HasPrefix(strings.ToLower(p.DNSName), lower) {
			return string(p.ID)
		}
	}
	return nameOrIP
}

// StatusJSON returns the ipnstate.Status document ("{}" before Start).
func (e *Engine) StatusJSON() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.lb == nil {
		return "{}"
	}
	b, err := json.Marshal(e.lb.Status())
	if err != nil {
		return "{}"
	}
	return string(b)
}

// Socks5ProxyURL is the URL Rethink adds to its proxy chain to reach the
// tailnet.
func (e *Engine) Socks5ProxyURL() string { return "socks5://" + Socks5Addr }

// startDNSRelay serves plain DNS53 on DNSAddr, answering via the engine's
// DNS manager (the same path netstack uses for TUN-intercepted queries).
// This gives the app a loopback address it can hand to firestack's
// AddDNSProxy, since socks5 proxies cannot carry per-proxy DNS transports
// (AddProxyDNS requires one), and dialing magicDNSResolverIP from inside
// the process would bypass the TUN-layer interception and blackhole.
func (e *Engine) startDNSRelay(ctx context.Context) error {
	udpLn, err := net.ListenPacket("udp", DNSAddr)
	if err != nil {
		return fmt.Errorf("udp listen: %w", err)
	}
	tcpLn, err := net.Listen("tcp", DNSAddr)
	if err != nil {
		udpLn.Close()
		return fmt.Errorf("tcp listen: %w", err)
	}
	e.dnsUdp = udpLn
	e.dnsTcp = tcpLn

	go func() {
		<-ctx.Done()
		udpLn.Close()
		tcpLn.Close()
	}()
	go e.serveDNSUDP(ctx, udpLn)
	go e.serveDNSTCP(ctx, tcpLn)
	log.Printf("ts-dns: serving %s via dns.Manager", DNSAddr)
	return nil
}

func (e *Engine) serveDNSUDP(ctx context.Context, pc net.PacketConn) {
	buf := make([]byte, 65535)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			return // listener closed on stop
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		go e.relayUDPQuery(ctx, pc, client, query)
	}
}

func (e *Engine) relayUDPQuery(ctx context.Context, pc net.PacketConn, client net.Addr, query []byte) {
	resp, err := e.dns.Query(ctx, query, "udp", clientAddrPort(client))
	if err != nil {
		log.Printf("ts-dns: udp query: %v", err)
		return
	}
	if _, err := pc.WriteTo(resp, client); err != nil {
		log.Printf("ts-dns: udp write client: %v", err)
	}
}

func (e *Engine) serveDNSTCP(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed on stop
		}
		go e.handleDNSTCPConn(ctx, conn)
	}
}

// handleDNSTCPConn relays length-prefixed DNS messages (RFC 1035 §4.2.2),
// one exchange per inbound message, until the client disconnects.
func (e *Engine) handleDNSTCPConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	for {
		query, err := readTCPMessage(conn)
		if err != nil {
			return
		}
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		resp, err := e.dns.Query(tctx, query, "tcp", clientAddrPort(conn.RemoteAddr()))
		cancel()
		if err != nil {
			log.Printf("ts-dns: tcp query: %v", err)
			return
		}
		if err := writeTCPMessage(conn, resp); err != nil {
			return
		}
	}
}

// clientAddrPort best-effort converts a peer address for dns.Manager's
// from parameter (used only for logging/caching); falls back to loopback.
func clientAddrPort(addr net.Addr) netip.AddrPort {
	switch a := addr.(type) {
	case *net.UDPAddr:
		if ip, ok := netip.AddrFromSlice(a.IP); ok {
			return netip.AddrPortFrom(ip.Unmap(), uint16(a.Port))
		}
	case *net.TCPAddr:
		if ip, ok := netip.AddrFromSlice(a.IP); ok {
			return netip.AddrPortFrom(ip.Unmap(), uint16(a.Port))
		}
	}
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 0)
}

// readTCPMessage reads one 2-byte-prefixed DNS message; returns io.EOF on a
// clean client disconnect between messages.
func readTCPMessage(c net.Conn) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
		return nil, err
	}
	msgLen := int(lenBuf[0])<<8 | int(lenBuf[1])
	if msgLen == 0 {
		return nil, fmt.Errorf("ts-dns: zero-length tcp dns message")
	}
	msg := make([]byte, msgLen)
	if _, err := io.ReadFull(c, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func writeTCPMessage(c net.Conn, msg []byte) error {
	if len(msg) > 0xFFFF {
		return fmt.Errorf("ts-dns: tcp dns message too long: %d", len(msg))
	}
	out := make([]byte, 2+len(msg))
	out[0] = byte(len(msg) >> 8)
	out[1] = byte(len(msg))
	copy(out[2:], msg)
	_, err := c.Write(out)
	return err
}

func (e *Engine) watchNotifications(ctx context.Context) {
	// State + BrowseToURL changes are always delivered to watchers;
	// the Initial* opts request the corresponding first-message fields.
	mask := ipn.NotifyInitialState | ipn.NotifyInitialNetMap |
		ipn.NotifyInitialHealthState
	// blocks until ctx is canceled or the watch is closed
	e.lb.WatchNotifications(ctx, mask, func() {}, func(n *ipn.Notify) bool {
		if n.State != nil && e.cb != nil {
			e.cb.OnStateChange(int(*n.State))
		}
		if n.BrowseToURL != nil && e.cb != nil {
			e.cb.OnBrowseToURL(*n.BrowseToURL)
		}
		if n.Health != nil && len(n.Health.Warnings) > 0 && e.cb != nil {
			msgs := make([]string, 0, len(n.Health.Warnings))
			for _, w := range n.Health.Warnings {
				if w.Text != "" {
					msgs = append(msgs, w.Text)
				}
			}
			if len(msgs) > 0 {
				e.cb.OnError(strings.Join(msgs, "; "))
			}
		}
		return true
	})
}

func (e *Engine) reportErr(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("ts-engine: %s", msg)
	if e.cb != nil {
		e.cb.OnError(msg)
	}
}

// deadLogID matches libtailscale's fallback private log ID.
func deadLogID() logid.PrivateID {
	var id logid.PrivateID
	id.UnmarshalText([]byte("dead0000dead0000dead0000dead0000dead0000dead0000dead0000dead0000"))
	return id
}

// setupPersistentLog tees the default logger into a rotating file so engine
// diagnostics survive Rethink's small in-memory console ring.
func setupPersistentLog(path string) {
	const maxBytes = 2 << 20 // 2 MiB; truncate when doubled over half
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxBytes/2 {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
}

// loadOrCreateLogID returns a stable private log ID persisted under stateDir.
func loadOrCreateLogID(stateDir string) (logid.PrivateID, error) {
	var zero logid.PrivateID
	if stateDir == "" {
		return logid.NewPrivateID()
	}
	path := filepath.Join(stateDir, "logid.txt")
	if b, err := os.ReadFile(path); err == nil {
		var id logid.PrivateID
		if id.UnmarshalText(b) == nil {
			return id, nil
		}
	}
	id, err := logid.NewPrivateID()
	if err != nil {
		return zero, err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return zero, err
	}
	txt, _ := id.MarshalText()
	if err := os.WriteFile(path, txt, 0o600); err != nil {
		return zero, err
	}
	return id, nil
}

// ---- interface-list bridge (Java -> netmon), mirroring tailscale-android's
// libtailscale/ifaceparse ----

type addrJSON struct {
	IP        string `json:"ip"`
	PrefixLen int    `json:"prefixLen"`
}

type ifaceJSON struct {
	Name      string     `json:"name"`
	Index     int        `json:"index"`
	MTU       int        `json:"mtu"`
	Up        bool       `json:"up"`
	Broadcast bool       `json:"broadcast"`
	Loopback  bool       `json:"loopback"`
	PointToPt bool       `json:"pointToPoint"`
	Multicast bool       `json:"multicast"`
	Addrs     []addrJSON `json:"addrs"`
}

// parseInterfacesJSON converts the JSON payload produced by Kotlin's
// java.net.NetworkInterface enumeration into netmon.Interfaces.
func parseInterfacesJSON(b string) ([]netmon.Interface, error) {
	trim := strings.TrimSpace(b)
	if trim == "" || (!strings.HasPrefix(trim, "[") && !strings.HasPrefix(trim, "{")) {
		return nil, fmt.Errorf("interfaces: not a JSON payload")
	}
	var in []ifaceJSON
	if err := json.Unmarshal([]byte(trim), &in); err != nil {
		return nil, fmt.Errorf("interfaces: %w", err)
	}
	out := make([]netmon.Interface, 0, len(in))
	for _, it := range in {
		if it.Name == "" {
			continue
		}
		nif := netmon.Interface{
			Interface: &net.Interface{
				Name:  it.Name,
				Index: it.Index,
				MTU:   it.MTU,
			},
			AltAddrs: []net.Addr{},
		}
		if it.Up {
			nif.Flags |= net.FlagUp
		}
		if it.Broadcast {
			nif.Flags |= net.FlagBroadcast
		}
		if it.Loopback {
			nif.Flags |= net.FlagLoopback
		}
		if it.PointToPt {
			nif.Flags |= net.FlagPointToPoint
		}
		if it.Multicast {
			nif.Flags |= net.FlagMulticast
		}
		for _, a := range it.Addrs {
			na, err := netip.ParseAddr(a.IP)
			if err != nil {
				continue
			}
			ip := net.IP(na.AsSlice())
			var addr net.Addr
			if zone := na.Zone(); zone != "" {
				// zoned addresses cannot be represented as *net.IPNet
				addr = &net.IPAddr{IP: ip, Zone: zone}
			} else {
				bits := 128
				if na.Is4() {
					bits = 32
				}
				if a.PrefixLen < 0 || a.PrefixLen > bits {
					addr = &net.IPAddr{IP: ip}
				} else {
					addr = &net.IPNet{IP: ip, Mask: net.CIDRMask(a.PrefixLen, bits)}
				}
			}
			nif.AltAddrs = append(nif.AltAddrs, addr)
		}
		out = append(out, nif)
	}
	return out, nil
}
