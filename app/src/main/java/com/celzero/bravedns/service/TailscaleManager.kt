/*
 * Copyright 2025 RethinkDNS and its authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package com.celzero.bravedns.service

import android.content.Context
import androidx.lifecycle.MutableLiveData
import com.celzero.bravedns.util.Logger
import com.celzero.bravedns.util.Utilities
import com.celzero.firestack.tailscale.Callback
import com.celzero.firestack.tailscale.Engine
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import org.koin.core.component.KoinComponent
import org.koin.core.component.inject

/**
 * Owns the embedded userspace tailscaled engine (gomobile AAR built from
 * /tailscale).
 *
 * The engine serves SOCKS5 on 127.0.0.1:1055 with egress via the tailnet;
 * [GoVpnAdapter] adds that endpoint to firestack's proxy chain whenever this
 * manager reports [TsState.RUNNING], so tailnet and exit-node traffic flows
 * through Rethink's regular per-app proxy routing.
 *
 * Lifecycle: started alongside the VPN tunnel ([onVpnStarted]) when
 * [PersistentState.tailscaleEnabled] is set; stopped on tunnel teardown
 * ([onVpnStopped]). Login state persists across restarts in the engine state
 * dir, so no re-login is needed unless the user logs out or the node key
 * expires (state NEEDS_LOGIN re-triggers the interactive login URL).
 */
class TailscaleManager private constructor(private val context: Context) : KoinComponent {

    private val persistentState by inject<PersistentState>()

    companion object {
        const val TAG = "TailscaleManager"

        // proxy id within the firestack tunnel; apps mapped to this id route
        // through the tailnet (mirrors ProxyManager.ID_ORBOT_BASE et al.)
        const val ID_TS_BASE = "TS"
        const val TS_PROXY_NAME = "Tailscale"

        // default loopback endpoint served by the engine; kept in sync with
        // tailscale.Socks5Addr in /tailscale/engine.go
        const val TS_SOCKS5_ADDR = "127.0.0.1:1055"

        // loopback plain-DNS endpoint served by the engine; kept in sync with
        // tailscale.DNSAddr in /tailscale/engine.go
        const val TS_DNS_ADDR = "127.0.0.1:1053"

        // dns transport id; MUST differ from ID_TS_BASE — firestack sets a
        // dns53 transport's relay to the proxy sharing its id, which would
        // route loopback queries through the tailnet socks5 endpoint
        // (CONNECT-only, no UDP) where they time out
        const val ID_TS_DNS = "TSDNS"

        @Volatile
        private var instance: TailscaleManager? = null

        fun getInstance(context: Context): TailscaleManager {
            return instance ?: synchronized(this) {
                instance ?: TailscaleManager(context.applicationContext).also { instance = it }
            }
        }
    }

    /** Mirrors ipn.State values from tailscale.com/ipn (see engine.go). */
    enum class TsState(val id: Int) {
        NO_STATE(0),
        IN_USE_OTHER_USER(1),
        NEEDS_LOGIN(2),
        NEEDS_MACHINE_AUTH(3),
        STOPPED(4),
        STARTING(5),
        RUNNING(6);

        val isUsable: Boolean
            get() = this == RUNNING

        val needsLogin: Boolean
            get() = this == NEEDS_LOGIN || this == NEEDS_MACHINE_AUTH

        companion object {
            fun of(id: Int): TsState {
                return entries.find { it.id == id } ?: NO_STATE
            }
        }
    }

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val engineMutex = Mutex()

    private var engine: Engine? = null

    /** Last reported ipn.State; observed by settings UI. */
    val stateObservable = MutableLiveData(TsState.NO_STATE)

    /** Interactive login URL awaiting user action; cleared once consumed. */
    val loginUrlObservable = MutableLiveData<String?>(null)

    /** Last error/health warning text; observed by settings UI. */
    val errorObservable = MutableLiveData<String?>(null)

    /** True while the engine process is up (regardless of login state). */
    var isEngineUp: Boolean = false
        private set

    /**
     * Callback bridge; holds a strong reference for the lifetime of the
     * engine (gomobile keeps only weak references to Java peers).
     */
    private val callback =
        object : Callback {
            override fun onStateChange(state: Long) {
                val s = TsState.of(state.toInt())
                Logger.i(TAG, "ts state change: $s")
                stateObservable.postValue(s)
                persistentState.tailscaleState = s.id
                if (s.isUsable) {
                    // Connection established: drop any transient health warning
                    // (e.g. "Tailscale is starting. Please wait.") that Go posted
                    // but never retracted — watchNotifications only emits OnError
                    // while warnings exist, so the cleared warning leaves a stale
                    // message stuck in the UI. Real errors re-post via OnError.
                    errorObservable.postValue(null)
                    // auth key is single-use; only forget it once we are actually
                    // logged in. If auth failed (engine stalls at NeedsLogin) the
                    // key stays so the user can retry with a fresh one.
                    if (persistentState.tailscaleAuthKey.isNotEmpty()) {
                        persistentState.tailscaleAuthKey = ""
                    }
                    // tailnet reachable; make sure the tunnel carries the proxy
                    scope.launch { VpnController.onTailscaleReady() }
                }
            }

            override fun onBrowseToURL(url: String?) {
                Logger.i(TAG, "ts browse-to-url: $url")
                if (!url.isNullOrEmpty()) loginUrlObservable.postValue(url)
            }

            override fun onError(message: String?) {
                Logger.w(TAG, "ts err: $message")
                if (!message.isNullOrEmpty()) errorObservable.postValue(message)
            }

            /**
             * Go's net.Interfaces is blocked for apps on Android SDK 30+, so
             * netmon sources the interface list from Java instead (same as
             * tailscale-android). Schema consumed by engine.go's
             * parseInterfacesJSON.
             */
            override fun getInterfacesAsJson(): String {
                return try {
                    val sb = StringBuilder("[")
                    val ifaces = java.net.NetworkInterface.getNetworkInterfaces()
                    var first = true
                    while (ifaces?.hasMoreElements() == true) {
                        val nif = ifaces.nextElement()
                        if (!first) sb.append(',')
                        first = false
                        sb.append("{")
                        .append("\"name\":").append(nif.name.toJson())
                        .append(",\"index\":").append(nif.index)
                        .append(",\"mtu\":").append(nif.mtu)
                        .append(",\"up\":").append(nif.isUp)
                        .append(",\"broadcast\":").append(nif.isUp && !nif.isLoopback && !nif.isPointToPoint)
                        .append(",\"loopback\":").append(nif.isLoopback)
                        .append(",\"pointToPoint\":").append(nif.isPointToPoint)
                        .append(",\"multicast\":").append(nif.supportsMulticast())
                        sb.append(",\"addrs\":[")
                        var addrFirst = true
                        for (ia in nif.interfaceAddresses) {
                            val ip = ia.address?.hostAddress ?: continue
                            if (!addrFirst) sb.append(',')
                            addrFirst = false
                            // strip v6 zone (%wlan0) — not parseable by Go's
                            // netip when embedded in JSON payloads here
                            val cleanIp = ip.substringBefore('%')
                            sb.append("{")
                            .append("\"ip\":").append(cleanIp.toJson())
                            .append(",\"prefixLen\":").append(ia.networkPrefixLength)
                            .append("}")
                        }
                        sb.append(']')
                        sb.append('}')
                    }
                    sb.append(']')
                    sb.toString()
                } catch (e: Exception) {
                    Logger.w(TAG, "ts get-interfaces failure: ${e.message}")
                    "[]"
                }
            }

            private fun String.toJson(): String {
                val escaped = replace("\\", "\\\\").replace("\"", "\\\"")
                return "\"$escaped\""
            }
        }

    fun isEnabled(): Boolean = persistentState.tailscaleEnabled

    fun currentState(): TsState = stateObservable.value ?: TsState.NO_STATE

    fun currentLoginUrl(): String? = loginUrlObservable.value

    /**
     * Starts the embedded engine if enabled and not yet running. Called when
     * the VPN tunnel comes up; safe to call repeatedly.
     */
    suspend fun onVpnStarted() {
        if (!isEnabled()) {
            Logger.v(TAG, "ts disabled, skip start")
            return
        }
        startEngineInternal()
    }

    /** Stops the engine when the VPN tunnel goes down. */
    suspend fun onVpnStopped() {
        if (!isEngineUp) return
        stopEngineInternal()
    }

    /** User-initiated toggle from the UI. */
    suspend fun setEnabled(enabled: Boolean) {
        persistentState.tailscaleEnabled = enabled
        if (enabled) {
            startEngineInternal()
        } else {
            // drop the proxy from the tunnel before tearing the engine down
            VpnController.removeTailscaleProxy()
            stopEngineInternal()
            stateObservable.postValue(TsState.NO_STATE)
        }
    }

    /**
     * Re-creates the engine so a fresh Start() can consume the given auth
     * key (ipn.Options.AuthKey is only honored at backend start).
     */
    suspend fun restartEngineWithAuthKey(authKey: String): Pair<Boolean, String> {
        persistentState.tailscaleAuthKey = authKey
        return try {
            stopEngineInternal()
            startEngineInternal()
            Pair(true, "")
        } catch (e: Exception) {
            Logger.e(TAG, "ts auth-key restart failure: ${e.message}", e)
            Pair(false, e.message ?: "failure")
        }
    }

    /** Prepends https:// when the user omitted the scheme. */
    fun normalizeControlUrl(url: String): String {
        val u = url.trim().trimEnd('/')
        if (u.isEmpty()) return ""
        return if (u.startsWith("http://") || u.startsWith("https://")) u else "https://$u"
    }

    /**
     * Saves the coordination server URL and restarts the engine if it is
     * running — ControlURL is only read at backend start.
     */
    suspend fun setControlUrl(url: String): Pair<Boolean, String> {
        val normalized = normalizeControlUrl(url)
        persistentState.tailscaleControlUrl = normalized
        if (!isEnabled()) {
            return Pair(true, "")
        }
        return try {
            stopEngineInternal()
            startEngineInternal()
            Pair(true, "")
        } catch (e: Exception) {
            Logger.e(TAG, "ts control-url restart failure: ${e.message}", e)
            Pair(false, e.message ?: "failure")
        }
    }

    /** Begins the browser-based login flow (interactive auth). */
    suspend fun loginInteractive(): Pair<Boolean, String> {
        val eng = engine
        if (eng == null || !isEngineUp) {
            return Pair(false, "engine not running")
        }
        return try {
            eng.loginInteractive()
            Pair(true, "")
        } catch (e: Exception) {
            Logger.e(TAG, "ts login-interactive failure: ${e.message}", e)
            Pair(false, e.message ?: "failure")
        }
    }

    /** Logs out and forgets node identity. */
    suspend fun logout(): Pair<Boolean, String> {
        val eng = engine
        if (eng == null || !isEngineUp) {
            return Pair(false, "engine not running")
        }
        return try {
            eng.logout()
            persistentState.tailscaleAuthKey = "" // never reuse stale keys
            Pair(true, "")
        } catch (e: Exception) {
            Logger.e(TAG, "ts logout failure: ${e.message}", e)
            Pair(false, e.message ?: "failure")
        }
    }

    /** Applies the exit-node selection stored in prefs ("" = none). */
    suspend fun applyExitNode(node: String): Pair<Boolean, String> {
        persistentState.tailscaleExitNode = node
        val eng = engine
        if (eng == null || !isEngineUp) {
            return Pair(false, "engine not running")
        }
        return try {
            eng.setExitNode(node)
            Pair(true, "")
        } catch (e: Exception) {
            Logger.e(TAG, "ts set-exit-node($node) failure: ${e.message}", e)
            Pair(false, e.message ?: "failure")
        }
    }

    fun getExitNode(): String = persistentState.tailscaleExitNode

    suspend fun setMagicDns(enabled: Boolean): Pair<Boolean, String> {
        persistentState.tailscaleMagicDns = enabled
        val eng = engine
        if (eng == null || !isEngineUp) {
            return Pair(false, "engine not running")
        }
        return try {
            eng.setCorpDNS(enabled)
            Pair(true, "")
        } catch (e: Exception) {
            Logger.e(TAG, "ts set-magic-dns($enabled) failure: ${e.message}", e)
            Pair(false, e.message ?: "failure")
        }
    }

    fun isMagicDnsEnabled(): Boolean = persistentState.tailscaleMagicDns

    fun magicDnsSuffix(): String {
        return try {
            engine?.magicDNSSuffix() ?: ""
        } catch (e: Exception) {
            ""
        }
    }

    data class NodeInfo(val name: String, val ipv4: String, val ipv6: String)

    fun nodeInfo(): NodeInfo {
        return try {
            val json = engine?.nodeInfo() ?: "{}"
            val obj = org.json.JSONObject(json)
            NodeInfo(
                name = obj.optString("name", ""),
                ipv4 = obj.optString("ipv4", ""),
                ipv6 = obj.optString("ipv6", "")
            )
        } catch (e: Exception) {
            NodeInfo("", "", "")
        }
    }

    /** Raw BackendState JSON for diagnostics screens. */
    fun statusJson(): String {
        return try {
            engine?.statusJSON() ?: "{}"
        } catch (e: Exception) {
            "{}"
        }
    }

    /**
     * The socks5:// URL to add to the firestack proxy chain; empty when the
     * engine is not up.
     */
    fun proxyUrl(): String {
        return if (isEngineUp) "socks5://$TS_SOCKS5_ADDR" else ""
    }

    private suspend fun startEngineInternal() {
        engineMutex.withLock {
            if (isEngineUp) {
                Logger.i(TAG, "ts engine already up")
                return
            }
            if (persistentState.routeRethinkInRethink) {
                Logger.w(TAG, "ts skip start: incompatible with routeRethinkInRethink")
                errorObservable.postValue("Tailscale is incompatible with Route Rethink in Rethink")
                return
            }
            try {
                stateObservable.postValue(TsState.STARTING)
                val stateDir = Utilities.getTailscaleStateDir(context)
                // gomobile binds Go's NewEngine as Engine's constructor
                val eng = Engine(stateDir.absolutePath, callback)
                val controlUrl = persistentState.tailscaleControlUrl.trim()
                Logger.i(TAG, "ts starting engine (control: ${controlUrl.ifEmpty { "saas" }})")
                val hostname = persistentState.tailscaleHostname.trim()
                val authKey = persistentState.tailscaleAuthKey.trim()
                eng.start(hostname, controlUrl, authKey)
                engine = eng
                isEngineUp = true
                // query actual state from the engine instead of relying on
                // the watchNotifications goroutine which may not have posted yet
                val initState = TsState.of(eng.currentState().toInt())
                stateObservable.postValue(initState)
                Logger.i(TAG, "ts engine started (state: $initState, control: ${controlUrl.ifEmpty { "saas" }})")
            } catch (e: Exception) {
                Logger.e(TAG, "ts engine start failure: ${e.message}", e)
                isEngineUp = false
                stateObservable.postValue(TsState.NO_STATE)
                errorObservable.postValue(e.message ?: "start failure")
            }
        }
    }

    private suspend fun stopEngineInternal() {
        engineMutex.withLock {
            if (!isEngineUp) return
            try {
                engine?.stop()
                Logger.i(TAG, "ts engine stopped")
            } catch (e: Exception) {
                Logger.e(TAG, "ts engine stop failure: ${e.message}", e)
            } finally {
                engine = null
                isEngineUp = false
            }
        }
    }

    /**
     * Re-applies prefs (exit node etc.) after the engine reaches a usable
     * state; called by the adapter when the proxy gets added to the tunnel.
     */
    suspend fun applyPendingPrefs() {
        val eng = engine ?: return
        try {
            val exitNode = persistentState.tailscaleExitNode
            if (exitNode.isNotEmpty()) eng.setExitNode(exitNode)
            val routes = persistentState.tailscaleAdvertiseRoutes
            if (routes.isNotEmpty()) eng.setAdvertiseRoutes(routes)
            eng.setShieldsUp(persistentState.tailscaleShieldsUp)
            eng.setAcceptRoutes(persistentState.tailscaleAcceptRoutes)
            eng.setCorpDNS(persistentState.tailscaleMagicDns)
        } catch (e: Exception) {
            Logger.w(TAG, "ts apply pending prefs err: ${e.message}")
        }
    }

    fun scopeForTest(): CoroutineScope = scope
}
