package dev.agentmesh.android

import dev.agentmesh.android.derive.QuotaTone
import dev.agentmesh.android.derive.SidebarHost
import dev.agentmesh.android.derive.buildProjects
import dev.agentmesh.android.derive.helpersLabel
import dev.agentmesh.android.derive.leadRows
import dev.agentmesh.android.derive.projectKey
import dev.agentmesh.android.derive.quotaLines
import dev.agentmesh.android.derive.searchProjects
import dev.agentmesh.android.derive.tone
import dev.agentmesh.android.derive.usageView
import dev.agentmesh.android.derive.visibleProjects
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.AgentRole
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.AgyUsageRow
import dev.agentmesh.gen.agentmesh.v1.ClaudeUsage
import dev.agentmesh.gen.agentmesh.v1.ClaudeUsageWindow
import dev.agentmesh.gen.agentmesh.v1.GetUsageResponse
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/** The drawer's projects: the desktop's `sidebar.test.ts` cases, on the same rules. */
class ProjectsTest {
    private fun a(
        id: String, repo: String, run: String = "r1", role: AgentRole = AgentRole.AGENT_ROLE_WORKER, parent: String = "",
        state: AgentState = AgentState.AGENT_STATE_IDLE, at: Long = 1_000, archived: Boolean = false,
    ): Agent = agent(id, runId = run, role = role, parent = parent, state = state, at = at).toBuilder().setRepo(repo).setArchived(archived).build()

    private fun host(id: String = "h1", name: String = "wsl", runs: List<dev.agentmesh.gen.agentmesh.v1.Run>, agents: List<Agent>, waiting: Set<String> = emptySet(), up: Boolean = true) =
        SidebarHost(id, name, up, false, runs.associateBy { it.id }, agents.associateBy { it.id }, waiting)

    private val shop = "/home/u/shop-api"
    private val web = "/home/u/web"

    @Test fun `a run sits under its leads project and the same folder on two hosts is two projects`() {
        val runs = listOf(run("r1"))
        val ag = listOf(a("lead", shop, role = AgentRole.AGENT_ROLE_ROOT), a("w1", shop, parent = "lead"))
        val rows = buildProjects(listOf(host("h1", "wsl", runs, ag), host("h2", "vps", runs, ag)), emptyMap())
        assertEquals(listOf(projectKey("h1", shop), projectKey("h2", shop)).toSet(), rows.map { it.key }.toSet())
        assertEquals(setOf("shop-api"), rows.map { it.name }.toSet())
        assertEquals(listOf("r1"), rows[0].runs.map { it.run.id })
    }

    @Test fun `workers fold under their lead and reviewers and askers become helpers at any depth`() {
        val ag = listOf(
            a("lead", shop, role = AgentRole.AGENT_ROLE_ROOT),
            a("w1", shop, parent = "lead", at = 2_000),
            a("rev1", shop, role = AgentRole.AGENT_ROLE_REVIEWER, parent = "w1", at = 3_000),
            a("rev2", shop, role = AgentRole.AGENT_ROLE_REVIEWER, parent = "w1", at = 4_000),
            a("ask", shop, role = AgentRole.AGENT_ROLE_ASKER, parent = "lead", at = 5_000),
        )
        val row = leadRows(host(runs = listOf(run("r1")), agents = ag)).single()
        assertEquals("lead", row.lead!!.id)
        assertEquals(listOf("w1"), row.workers.map { it.agent.id })
        assertEquals(listOf("rev1", "rev2"), row.workers[0].helpers.map { it.id })
        assertEquals(listOf("ask"), row.helpers.map { it.id })
        assertEquals("2 reviews", helpersLabel(row.workers[0].helpers))
        assertEquals("1 asker", helpersLabel(row.helpers))
        assertEquals("1 review, 2 askers", helpersLabel(listOf(ag[2], ag[4], ag[4])))
        assertEquals("", helpersLabel(emptyList()))
    }

    @Test fun `an archived agent is left out and its children are lifted into its place`() {
        val ag = listOf(
            a("lead", shop, role = AgentRole.AGENT_ROLE_ROOT),
            a("old", shop, parent = "lead", archived = true, at = 2_000),
            a("kid", shop, parent = "old", at = 3_000),
        )
        assertEquals(listOf("kid"), leadRows(host(runs = listOf(run("r1")), agents = ag)).single().workers.map { it.agent.id })
        // A run whose agents are all archived is not shown.
        assertTrue(leadRows(host(runs = listOf(run("r1")), agents = listOf(a("x", shop, archived = true)))).isEmpty())
    }

    @Test fun `a worker in another repo lists the run under that project as also working there`() {
        val ag = listOf(a("lead", shop, role = AgentRole.AGENT_ROLE_ROOT), a("w", web, parent = "lead"))
        val rows = buildProjects(listOf(host(runs = listOf(run("r1")), agents = ag)), emptyMap())
        val p = rows.single { it.repo == web }
        assertTrue(p.runs.isEmpty())
        assertEquals(listOf("r1"), p.also.map { it.run.id })
        assertTrue(leadRows(host(runs = listOf(run("r1")), agents = ag)).single().workers.single().elsewhere)
    }

    @Test fun `closed and finished runs fold under earlier and add no needs`() {
        val ag = listOf(
            a("l1", shop, run = "r1", role = AgentRole.AGENT_ROLE_ROOT, state = AgentState.AGENT_STATE_EXITED),
            a("l2", shop, run = "r2", role = AgentRole.AGENT_ROLE_ROOT, state = AgentState.AGENT_STATE_WORKING),
            a("l3", shop, run = "r3", role = AgentRole.AGENT_ROLE_ROOT, state = AgentState.AGENT_STATE_NEEDS_INPUT),
        )
        val runs = listOf(run("r1"), run("r2"), run("r3", status = RunStatus.RUN_STATUS_CLOSED))
        val p = buildProjects(listOf(host(runs = runs, agents = ag)), emptyMap()).single()
        assertEquals(listOf("r2"), p.runs.map { it.run.id })
        assertEquals(setOf("r1", "r3"), p.earlier.map { it.run.id }.toSet())
        assertEquals(0, p.needs)
    }

    @Test fun `the order is pins in pin order, then what waits on the user, then the latest activity`() {
        val ag = listOf(
            a("a", "/p/old", run = "r1", role = AgentRole.AGENT_ROLE_ROOT, at = 1_000),
            a("b", "/p/new", run = "r2", role = AgentRole.AGENT_ROLE_ROOT, at = 9_000),
            a("c", "/p/needs", run = "r3", role = AgentRole.AGENT_ROLE_ROOT, at = 2_000, state = AgentState.AGENT_STATE_NEEDS_INPUT),
        )
        val runs = listOf(run("r1", at = 1_000), run("r2", at = 9_000), run("r3", at = 2_000))
        val h = host(runs = runs, agents = ag)
        assertEquals(listOf("needs", "new", "old"), buildProjects(listOf(h), emptyMap()).map { it.name })
        // A pin comes first, in the order it was pinned, and a pinned folder with no run is a row.
        assertEquals(listOf("old", "empty", "needs", "new"), buildProjects(listOf(h), mapOf("h1" to listOf("/p/old", "/p/empty"))).map { it.name })
        assertTrue(buildProjects(listOf(h), mapOf("h1" to listOf("/p/empty"))).first { it.name == "empty" }.empty)
    }

    @Test fun `a hidden project comes back while something in it waits on the user`() {
        val ag = listOf(a("a", shop, role = AgentRole.AGENT_ROLE_ROOT), a("b", web, run = "r2", role = AgentRole.AGENT_ROLE_ROOT, state = AgentState.AGENT_STATE_NEEDS_INPUT))
        val rows = buildProjects(listOf(host(runs = listOf(run("r1"), run("r2")), agents = ag)), emptyMap())
        val (shown, hidden) = visibleProjects(rows, setOf(projectKey("h1", shop), projectKey("h1", web)))
        assertEquals(listOf("web"), shown.map { it.name })
        assertEquals(listOf("shop-api"), hidden.map { it.name })
    }

    @Test fun `what the host says waits counts for the run`() {
        val ag = listOf(a("lead", shop, role = AgentRole.AGENT_ROLE_ROOT), a("w", shop, parent = "lead"))
        assertEquals(1, leadRows(host(runs = listOf(run("r1")), agents = ag, waiting = setOf("w"))).single().needs)
    }

    @Test fun `search keeps the projects and runs with every word`() {
        val ag = listOf(
            a("lead", shop, role = AgentRole.AGENT_ROLE_ROOT).toBuilder().setModel("opus").build(),
            a("l2", web, run = "r2", role = AgentRole.AGENT_ROLE_ROOT),
        )
        val rows = buildProjects(listOf(host(runs = listOf(run("r1", title = "Fix login"), run("r2", title = "Docs")), agents = ag)), emptyMap())
        assertEquals(2, searchProjects(rows, "").size)
        assertEquals(listOf("web"), searchProjects(rows, "WEB").map { it.name })
        assertEquals(listOf("shop-api"), searchProjects(rows, "login opus").map { it.name })
        assertTrue(searchProjects(rows, "nothing here").isEmpty())
        // A project whose name matches keeps all its runs.
        assertEquals(1, searchProjects(rows, "shop").single().runs.size)
    }

    @Test fun `the quota lines are Claude's windows and agy's fullest row, green then amber then red`() {
        val u = GetUsageResponse.newBuilder().setQuotaStop(90).setMinQuota(10)
            .setClaude(
                ClaudeUsage.newBuilder().addAtReserve("7 d").addAtReserve("Opus")
                    .addWindows(ClaudeUsageWindow.newBuilder().setKey("5h").setLabel("5 h").setUsed(40.0))
                    .addWindows(ClaudeUsageWindow.newBuilder().setKey("7d").setLabel("7 d").setUsed(92.0))
                    .addWindows(ClaudeUsageWindow.newBuilder().setKey("o").setLabel("Opus").setUsed(100.0)),
            )
            .addAgyRows(AgyUsageRow.newBuilder().setGroup("Gemini").setLabel("Pro").setLeft(60))
            .addAgyRows(AgyUsageRow.newBuilder().setGroup("Gemini").setLabel("Flash").setLeft(5))
        val lines = quotaLines(usageView(u.build()))
        assertEquals(listOf("Claude 5 h", "Claude 7 d", "Claude Opus", "agy Gemini Flash"), lines.map { it.label })
        assertEquals(listOf(QuotaTone.Ok, QuotaTone.Warn, QuotaTone.Over, QuotaTone.Warn), lines.map { it.tone })
        assertEquals(95.0, lines.last().used, 0.0)
        assertFalse(quotaLines(usageView(GetUsageResponse.getDefaultInstance())).isNotEmpty())
    }

    @Test fun `a typed path is made the way the runs name it`() {
        assertEquals("/home/u/shop", dev.agentmesh.android.ui.normalPath("/home/u//shop/"))
        assertEquals("/home/u/shop", dev.agentmesh.android.ui.normalPath("/home/u/./shop"))
        assertEquals("C:\\work\\app", dev.agentmesh.android.ui.normalPath("C:\\work\\app\\"))
        // Spaces round it go first, or the slash after them would be missed.
        assertEquals("/home/u/shop", dev.agentmesh.android.ui.normalPath("  /home/u/shop/  "))
    }
}
