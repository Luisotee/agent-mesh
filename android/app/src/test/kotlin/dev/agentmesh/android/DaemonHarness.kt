package dev.agentmesh.android

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.longOrNull
import java.io.File
import java.nio.file.Files
import java.util.concurrent.TimeUnit

/**
 * A real meshd on its own temporary state dir and a loopback TCP port, with a fake `claude` in its PTYs: nothing of
 * the developer's agent-mesh, herdr, Claude config or HOME is touched, as in daemon/e2e. The binaries are built by
 * `go build` once per test run into `app/build/e2e-bin`. Tests that need it are skipped when Go isn't found.
 */
class DaemonHarness private constructor(val root: File) : AutoCloseable {
    val state = File(root, "state")
    val home = File(root, "home").also { it.mkdirs() }
    private var meshd: Process? = null
    private val log = File(root, "meshd.log")

    /** The environment every process of the harness gets: its own HOME, nothing inherited but PATH. */
    private fun env(): Map<String, String> = mapOf("HOME" to home.path, "PATH" to System.getenv("PATH").orEmpty(), "AGENT_MESH_STATE" to state.path)

    fun start() {
        val cmd = listOf(
            bin("meshd").path, "--state", state.path, "--tcp", "127.0.0.1:0", "--term-launch", "detached", "--max-lifetime", "5m",
            "--term-binary", bin("mesh-term").path, "--meshctl", bin("meshctl").path, "--cli", "claude=${bin("claude").path}",
            "--agent-env", "HOME=${home.path}", "--agent-env", "PATH=${System.getenv("PATH").orEmpty()}",
        )
        val pb = ProcessBuilder(cmd).redirectErrorStream(true).redirectOutput(log)
        pb.environment().clear(); pb.environment().putAll(env())
        meshd = pb.start()
        val deadline = System.currentTimeMillis() + 20_000
        while (!(File(state, "meshd.json").exists() && File(state, "device.token").exists() && tcp().isNotEmpty())) {
            check(System.currentTimeMillis() < deadline && meshd!!.isAlive) { "meshd didn't come up:\n${log.readText()}" }
            Thread.sleep(50)
        }
    }

    private fun info(): JsonObject? = runCatching { Json.parseToJsonElement(File(state, "meshd.json").readText()).jsonObject }.getOrNull()
    fun tcp(): String = info()?.get("tcp")?.jsonPrimitive?.content.orEmpty()
    val url: String get() = "http://${tcp()}"
    val token: String get() = File(state, "device.token").readText().trim()
    val daemonId: String get() = info()!!["daemon_id"]!!.jsonPrimitive.content

    /** Runs `meshctl <args>` as the host's own device and returns its JSON output. */
    fun ctl(vararg args: String): JsonObject {
        val pb = ProcessBuilder(listOf(bin("meshctl").path) + args).redirectErrorStream(false)
        pb.environment().clear(); pb.environment().putAll(env())
        val p = pb.start()
        val out = p.inputStream.bufferedReader().readText()
        val err = p.errorStream.bufferedReader().readText()
        check(p.waitFor(30, TimeUnit.SECONDS) && p.exitValue() == 0) { "meshctl ${args.joinToString(" ")}: $err" }
        return Json.parseToJsonElement(out).jsonObject
    }

    /** A git repo with one commit, for agents' worktrees. */
    fun repo(name: String): File {
        val dir = File(root, name).also { it.mkdirs() }
        for (args in listOf(listOf("init", "-q", "-b", "main"), listOf("-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"))) {
            val p = ProcessBuilder(listOf("git") + args).directory(dir).redirectErrorStream(true).start()
            val out = p.inputStream.readBytes().decodeToString()
            check(p.waitFor() == 0) { "git $args: $out" }
        }
        return dir
    }

    /** Stops meshd and, through its `--max-lifetime`, whatever it started. */
    override fun close() {
        meshd?.let { it.destroy(); it.waitFor(10, TimeUnit.SECONDS); if (it.isAlive) it.destroyForcibly() }
        // The terminal host and the agents die with it: meshd was told to stop mesh-term with itself.
        val info = File(state, "term/info.json")
        runCatching { Json.parseToJsonElement(info.readText()).jsonObject["pid"]?.jsonPrimitive?.longOrNull }.getOrNull()?.let { pid ->
            ProcessHandle.of(pid).ifPresent { it.destroy() }
        }
        root.deleteRecursively()
    }

    companion object {
        private val goBin: File? by lazy {
            sequenceOf(System.getenv("AGENT_MESH_GO"), "${System.getProperty("user.home")}/.local/go/bin/go")
                .filterNotNull().map(::File).firstOrNull { it.canExecute() }
                ?: System.getenv("PATH").orEmpty().split(':').map { File(it, "go") }.firstOrNull { it.canExecute() }
        }
        private val repoRoot: File by lazy {
            generateSequence(File(System.getProperty("user.dir")).absoluteFile) { it.parentFile }.first { File(it, "daemon/go.mod").exists() }
        }
        private val bins: File by lazy {
            val out = File(System.getProperty("user.dir"), "build/e2e-bin").also { it.mkdirs() }
            val daemon = File(repoRoot, "daemon")
            for ((name, pkg) in mapOf("meshd" to "./cmd/meshd", "mesh-term" to "./cmd/mesh-term", "meshctl" to "./cmd/meshctl", "claude" to "./internal/testutil/fakeclaude")) {
                val pb = ProcessBuilder(goBin!!.path, "build", "-o", File(out, name).path, pkg).directory(daemon).redirectErrorStream(true)
                pb.environment()["CGO_ENABLED"] = "0"
                val p = pb.start()
                val text = p.inputStream.readBytes().decodeToString()
                check(p.waitFor() == 0) { "go build $pkg failed:\n$text" }
            }
            out
        }

        /** True when the daemon can be built here (Go found); the end-to-end tests are skipped otherwise. */
        val available: Boolean get() = goBin != null

        fun bin(name: String) = File(bins, name)

        fun create(): DaemonHarness = DaemonHarness(Files.createTempDirectory("amd").toFile()).also { it.state.parentFile.mkdirs() }
    }
}
