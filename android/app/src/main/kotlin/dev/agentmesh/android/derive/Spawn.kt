package dev.agentmesh.android.derive

import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentRole

/** The CLIs the host knows how to run, for the form's choices (it takes any adapter name the host has: a typed one is sent as it is). */
val CLI_KINDS = listOf("claude", "agy", "opencode", "copilot")

/** What the new-run and add-agent forms hold: SpawnAgent's fields as the desktop's form fills them (`spawn-dialogs.tsx`). */
data class SpawnForm(
    val repo: String = "",
    val slug: String = "",
    val role: AgentRole = AgentRole.AGENT_ROLE_ROOT,
    val kind: String = "claude",
    /** Empty for a lead that takes the CLI's default; a worker always has a tier or a model. */
    val tier: String = "",
    val model: String = "",
    val task: String = "",
)

private val SLUG = Regex("^[a-z0-9][a-z0-9-]*$")

/** The first agent of a run is its lead, every later one a worker. */
fun defaultRole(hasLead: Boolean): AgentRole = if (hasLead) AgentRole.AGENT_ROLE_WORKER else AgentRole.AGENT_ROLE_ROOT

/**
 * The tier a form starts on: none for a lead (the user's own session may take the CLI's default), else `mid` when the host
 * has one, else its first (the host has no default model, so a worker always runs on a tier or a named model).
 */
fun defaultTier(role: AgentRole, tiers: List<String>): String =
    if (role == AgentRole.AGENT_ROLE_ROOT) "" else if ("mid" in tiers) "mid" else tiers.firstOrNull().orEmpty()

/** A branch and agent name from a run's title: lower case, runs of other characters as one dash. */
fun slugOf(title: String): String = title.lowercase().replace(Regex("[^a-z0-9]+"), "-").trim('-').take(40)

/** Why the form can't be sent yet, or null when it can. */
fun spawnProblem(f: SpawnForm): String? = when {
    f.repo.isBlank() -> "Name the repository (a path on the host, or a folder of its repos directory)."
    f.slug.isBlank() -> "Give it a short name: it names the branch and the agent."
    !SLUG.matches(f.slug) -> "The name is lower-case letters, digits and dashes, starting with a letter or a digit."
    f.kind.isBlank() -> "Name the CLI."
    f.role != AgentRole.AGENT_ROLE_ROOT && f.tier.isBlank() && f.model.isBlank() -> "A worker runs on a tier or a model: choose one."
    else -> null
}

/** The repos agents of this host have worked in, most recently listed first, once each: suggestions beside the host's project list. */
fun knownRepos(agents: Collection<Agent>): List<String> = agents.map { it.repo }.filter { it.isNotEmpty() }.distinct()
