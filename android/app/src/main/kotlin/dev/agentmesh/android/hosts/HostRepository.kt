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

@Serializable
private data class Stored(val host: Host, val token: String)

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

    val hosts: StateFlow<List<Host>> get() = flow

    private fun load(): Stash = runCatching { json.decodeFromString<Stash>(file.readText()) }.getOrDefault(Stash())

    private fun save() {
        val tmp = File(file.parentFile, file.name + ".tmp")
        tmp.writeText(json.encodeToString(stash))
        if (!tmp.renameTo(file)) { file.delete(); tmp.renameTo(file) }
        flow.value = stash.hosts.map { it.host }
    }

    fun get(id: String): Host? = flow.value.firstOrNull { it.id == id }

    /** The token of a host, read when a request is made so a changed token takes effect on the next call. */
    fun token(id: String): String? = synchronized(lock) {
        stash.hosts.firstOrNull { it.host.id == id }?.let { runCatching { secrets.open(it.token) }.getOrNull() }
    }

    fun add(name: String, routes: List<Route>, token: String): Host = synchronized(lock) {
        val host = Host(UUID.randomUUID().toString(), name.trim(), routes)
        stash = Stash(stash.hosts + Stored(host, secrets.seal(token.trim())))
        save()
        host
    }

    /** Changes a host's name, routes or token (null keeps the token). */
    fun update(id: String, name: String, routes: List<Route>, token: String? = null) = synchronized(lock) {
        stash = Stash(stash.hosts.map {
            if (it.host.id != id) it else Stored(it.host.copy(name = name.trim(), routes = routes), if (token == null) it.token else secrets.seal(token.trim()))
        })
        save()
    }

    fun setDaemonId(id: String, daemonId: String) = synchronized(lock) {
        if (stash.hosts.firstOrNull { it.host.id == id }?.host?.daemonId == daemonId) return@synchronized
        stash = Stash(stash.hosts.map { if (it.host.id == id) it.copy(host = it.host.copy(daemonId = daemonId)) else it })
        save()
    }

    fun remove(id: String) = synchronized(lock) {
        stash = Stash(stash.hosts.filterNot { it.host.id == id })
        save()
    }
}
