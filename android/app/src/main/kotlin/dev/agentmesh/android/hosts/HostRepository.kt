package dev.agentmesh.android.hosts

import dev.agentmesh.android.net.Route
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.io.File
import java.util.UUID

/** A host the app knows: where to reach it, by [routes] in order of preference. The token is kept apart. */
@Serializable
data class Host(
    val id: String,
    val name: String,
    val routes: List<Route>,
    /** The daemon's stable id, learnt at the first handshake: a different one at the same address is another host. */
    val daemonId: String? = null,
)

/** [host] has its relay client tokens sealed (see [HostRepository.sealRoutes]); [token] is the sealed device token. */
@Serializable
private data class Stored(val host: Host, val token: String)

/** Marks a relay client token that is sealed; one without it was written in the clear by the first version and is sealed on the next save. */
private const val SEALED = "sealed:"

@Serializable
private data class Stash(val hosts: List<Stored> = emptyList())

/**
 * The host list, which can't live on a host (it comes before any connection): `hosts.json` in the app's private
 * storage, each device token sealed by a [SecretBox]. It is the one piece of UI state that isn't on a host.
 */
class HostRepository(private val file: File, private val secrets: SecretBox) {
    private val json = Json { ignoreUnknownKeys = true; encodeDefaults = true }
    private val lock = Any()
    private var stash: Stash = load()
    private val flow = MutableStateFlow(stash.hosts.map { it.host })

    init {
        // A first-version file holds relay tokens in the clear: seal them now rather than on some later save.
        if (stash.hosts.any { h -> h.host.routes.any { it is Route.Relay && !it.relayClientToken.startsWith(SEALED) && it.relayClientToken.isNotEmpty() } }) {
            runCatching { commit(stash) }
        }
    }

    val hosts: StateFlow<List<Host>> get() = flow

    /** Set when the saved list couldn't be read and was set aside: the user is told, and nothing overwrites it. */
    var recoveredFrom: File? = null
        private set

    private fun load(): Stash {
        if (!file.exists()) return Stash()
        return try {
            val raw = json.decodeFromString<Stash>(file.readText())
            // In memory a route's relay token is plain, like the device token once opened for a request.
            Stash(raw.hosts.map { it.copy(host = it.host.copy(routes = it.host.routes.map(::openRoute))) })
        } catch (e: Exception) {
            // Never let the next save replace an unreadable list: keep it beside the new one.
            val bad = File(file.parentFile, file.name + ".bad")
            recoveredFrom = if (file.renameTo(bad)) bad else null
            Stash()
        }
    }

    /** Writes [next] and only then makes it the current list: a failed write leaves memory and file as they were. */
    private fun commit(next: Stash) {
        save(next)
        stash = next
        flow.value = next.hosts.map { it.host }
    }

    private fun sealRoute(r: Route): Route =
        if (r is Route.Relay && r.relayClientToken.isNotEmpty() && !r.relayClientToken.startsWith(SEALED)) r.copy(relayClientToken = SEALED + secrets.seal(r.relayClientToken)) else r

    /** A token the Keystore can no longer open (a restored backup) comes back empty: the route then says to pair again. */
    private fun openRoute(r: Route): Route =
        if (r is Route.Relay && r.relayClientToken.startsWith(SEALED)) r.copy(relayClientToken = runCatching { secrets.open(r.relayClientToken.removePrefix(SEALED)) }.getOrDefault("")) else r

    private fun save(next: Stash) {
        val tmp = File(file.parentFile, file.name + ".tmp")
        java.io.FileOutputStream(tmp).use { out ->
            out.write(json.encodeToString(Stash(next.hosts.map { it.copy(host = it.host.copy(routes = it.host.routes.map(::sealRoute))) })).toByteArray())
            out.fd.sync()
        }
        java.nio.file.Files.move(tmp.toPath(), file.toPath(), java.nio.file.StandardCopyOption.ATOMIC_MOVE, java.nio.file.StandardCopyOption.REPLACE_EXISTING)
    }

    fun get(id: String): Host? = flow.value.firstOrNull { it.id == id }

    /** The token of a host, read when a request is made so a changed token takes effect on the next call. */
    fun token(id: String): String? = synchronized(lock) {
        stash.hosts.firstOrNull { it.host.id == id }?.let { runCatching { secrets.open(it.token) }.getOrNull() }
    }

    fun add(name: String, routes: List<Route>, token: String, daemonId: String? = null): Host = synchronized(lock) {
        val host = Host(UUID.randomUUID().toString(), name.trim(), routes, daemonId)
        commit(Stash(stash.hosts + Stored(host, secrets.seal(token.trim()))))
        host
    }

    /** Changes a host's name, routes or token (null keeps the token). */
    fun update(id: String, name: String, routes: List<Route>, token: String? = null) = synchronized(lock) {
        commit(Stash(stash.hosts.map {
            if (it.host.id != id) it else Stored(it.host.copy(name = name.trim(), routes = routes), if (token == null) it.token else secrets.seal(token.trim()))
        }))
    }

    fun setDaemonId(id: String, daemonId: String) = synchronized(lock) {
        val cur = stash.hosts.firstOrNull { it.host.id == id } ?: return@synchronized
        if (cur.host.daemonId == daemonId) return@synchronized
        commit(Stash(stash.hosts.map { if (it.host.id == id) it.copy(host = it.host.copy(daemonId = daemonId)) else it }))
    }

    fun remove(id: String) = synchronized(lock) {
        commit(Stash(stash.hosts.filterNot { it.host.id == id }))
    }
}
