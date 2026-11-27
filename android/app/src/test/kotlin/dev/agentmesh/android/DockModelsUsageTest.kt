package dev.agentmesh.android

import dev.agentmesh.android.derive.COST_WINDOWS
import dev.agentmesh.android.derive.DockHost
import dev.agentmesh.android.derive.dockTilesAcross
import dev.agentmesh.android.derive.activityAgo
import dev.agentmesh.android.derive.ago
import dev.agentmesh.android.derive.sentAgo
import dev.agentmesh.android.derive.costView
import dev.agentmesh.android.derive.dockTiles
import dev.agentmesh.android.derive.familiesOf
import dev.agentmesh.android.derive.pendingText
import dev.agentmesh.android.derive.resetText
import dev.agentmesh.android.derive.runsOn
import dev.agentmesh.android.derive.sentLabel
import dev.agentmesh.android.derive.tokens
import dev.agentmesh.android.derive.usageView
import dev.agentmesh.gen.agentmesh.v1.AgentState
import dev.agentmesh.gen.agentmesh.v1.AgyTierPick
import dev.agentmesh.gen.agentmesh.v1.AgyUsageRow
import dev.agentmesh.gen.agentmesh.v1.CheckModelsResponse
import dev.agentmesh.gen.agentmesh.v1.ClaudeUsage
import dev.agentmesh.gen.agentmesh.v1.ClaudeUsageWindow
import dev.agentmesh.gen.agentmesh.v1.CliCost
import dev.agentmesh.gen.agentmesh.v1.GetCostResponse
import dev.agentmesh.gen.agentmesh.v1.GetUsageResponse
import dev.agentmesh.gen.agentmesh.v1.KindModels
import dev.agentmesh.gen.agentmesh.v1.MessageType
import dev.agentmesh.gen.agentmesh.v1.ModelFamilyEntry
import dev.agentmesh.gen.agentmesh.v1.Reopen
import dev.agentmesh.gen.agentmesh.v1.RunStatus
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/** The live dock's tiles, the model line and the quota and cost rows: the desktop's `dock`, `models` and `usage` rules. */
class DockModelsUsageTest {
    @Test fun `the dock has a tile per live agent of an open run, the ones that need you first, then the latest`() {
        val runs = mapOf("r1" to run("r1"), "r2" to run("r2", status = RunStatus.RUN_STATUS_CLOSED))
        fun ag(id: String, st: AgentState, at: Long, runId: String = "r1", detail: String = "") = agent(id, runId = runId, state = st, detail = detail).toBuilder().setLastActivity(ts(at)).build()
        val agents = listOf(
            ag("old", AgentState.AGENT_STATE_WORKING, 1_000, detail = "Bash"), ag("new", AgentState.AGENT_STATE_WORKING, 9_000, detail = "Read"),
            ag("ask", AgentState.AGENT_STATE_NEEDS_INPUT, 2_000), ag("gone", AgentState.AGENT_STATE_EXITED, 5_000), ag("ret", AgentState.AGENT_STATE_RETIRED, 5_000),
            ag("closed", AgentState.AGENT_STATE_WORKING, 5_000, runId = "r2"),
        ).associateBy { it.id }
        val msgs = listOf(
            message("m1", from = "new", to = "lead", type = MessageType.MESSAGE_TYPE_INFO, at = 1_000),
            message("m2", from = "new", to = "lead", type = MessageType.MESSAGE_TYPE_PLAN, at = 2_000),
        ).associateBy { it.id }
        val tiles = dockTiles(runs, agents, msgs)
        assertEquals(listOf("ask", "new", "old"), tiles.map { it.agent.id })
        assertEquals(listOf("", "Read", "Bash"), tiles.map { it.activity })
        assertEquals("m2", tiles[1].lastSent?.id)
        assertEquals("sent a plan", sentLabel(tiles[1].lastSent!!))
    }

    @Test fun `how long ago, short`() {
        val now = 1_000_000_000L
        assertEquals("", ago(0, now))
        assertEquals("now", ago(now - 2_000, now))
        assertEquals("12 s", ago(now - 12_000, now))
        assertEquals("4 min", ago(now - 4 * 60_000, now))
        assertEquals("3 h", ago(now - 3 * 3_600_000, now))
        assertEquals("2 d", ago(now - 2 * 86_400_000, now))
    }

    @Test fun `the model line, a reopen under way, and the families the host lists`() {
        val a = agent("w1").toBuilder().setModel("sonnet").setEffort("high").build()
        assertEquals("claude · sonnet · high", runsOn(a))
        assertEquals("", pendingText(a))
        val moving = a.toBuilder().setPendingReopen(Reopen.newBuilder().setModel("opus").setEffort("high").setWhy("moved")).build()
        assertEquals("Switching to opus (moved)…", pendingText(moving))
        val effortOnly = a.toBuilder().setPendingReopen(Reopen.newBuilder().setModel("sonnet").setEffort("max")).build()
        assertEquals("Switching to max effort…", pendingText(effortOnly))
        val same = a.toBuilder().setPendingReopen(Reopen.newBuilder().setModel("sonnet").setEffort("high").setWhy("reset")).build()
        assertEquals("Reopening (reset)…", pendingText(same))
        val r = CheckModelsResponse.newBuilder().addKinds(KindModels.newBuilder().setKind("claude").addListed(ModelFamilyEntry.newBuilder().setFamily("opus")).addListed(ModelFamilyEntry.newBuilder().setFamily("sonnet"))).build()
        assertEquals(listOf("opus", "sonnet"), familiesOf(r, "claude"))
        assertEquals(emptyList<String>(), familiesOf(r, "agy"))
    }

    @Test fun `quota bars, the reserve and what is at it`() {
        val u = GetUsageResponse.newBuilder().setQuotaStop(90).setMinQuota(10)
            .setClaude(
                ClaudeUsage.newBuilder().setSource("oauth").addAtReserve("five-hour window")
                    .addWindows(ClaudeUsageWindow.newBuilder().setKey("five_hour").setLabel("five-hour window").setUsed(93.0).setResetsAt(ts(7_200_000)))
                    .addWindows(ClaudeUsageWindow.newBuilder().setKey("seven_day").setLabel("seven-day window").setUsed(40.0)),
            )
            .addAgyRows(AgyUsageRow.newBuilder().setGroup("g").setLabel("neon").setLeft(8).setResets("3h"))
            .addAgyRows(AgyUsageRow.newBuilder().setGroup("g").setLabel("flash").setLeft(60))
            .addAgyTiers(AgyTierPick.newBuilder().setTier("mid").setModel("hydrogen").setLeft(60).setHasLeft(true))
            .build()
        val v = usageView(u)
        assertEquals(90, v.reserve)
        assertEquals(listOf(true, false), v.claude.map { it.atReserve })
        assertEquals(listOf(true, false), v.agy.map { it.atReserve })
        assertEquals(listOf("Claude five-hour window", "agy g neon"), v.warnings)
        assertEquals(60, v.tiers[0].left)
        // No reserve at all once quota_stop is 100.
        assertTrue(usageView(u.toBuilder().setQuotaStop(100).build()).agy.none { it.atReserve })
    }

    @Test fun `when a window resets`() {
        val day = { ms: Long -> "Fri 10:00" }
        assertEquals("reset time unknown", resetText(null, 0, day))
        assertEquals("resetting", resetText(1_000, 5_000, day))
        assertEquals("resets in 30 min", resetText(30 * 60_000L, 0, day))
        assertEquals("resets in 2 h 10 min", resetText(130 * 60_000L, 0, day))
        assertEquals("resets Fri 10:00", resetText(3 * 86_400_000L, 0, day))
    }

    @Test fun `token counts the way mesh cost writes them`() {
        assertEquals("999", tokens(999)); assertEquals("340k", tokens(340_000)); assertEquals("1.2M", tokens(1_200_000)); assertEquals("1.0M", tokens(999_600))
    }

    @Test fun `the cost view leads with tokens and keeps the dollars beside them`() {
        val r = GetCostResponse.newBuilder().setScope("since 7d").setPricesAsOf("2026-11-01").setUsd(4.5).setTotalTokens(12_300_000).setTotalRead(40_000_000).setResets(2)
            .addClis(CliCost.newBuilder().setCli("claude").setCalls(40).setInput(1_000_000).setW1(2_000_000).setW5(300_000).setOutput(500_000).setRead(40_000_000).setTotal(3_800_000).setBasis("exact").setUsd(4.5))
            .build()
        val v = costView(r)
        assertEquals("12.3M tokens", v.tokens)
        assertEquals("input 1.0M, cache write 2.3M, output 500k; cache reads 40.0M apart", v.breakdown)
        assertEquals("≈ $4.50", v.usd)
        assertEquals(listOf("claude" to "exact"), v.bases)
        val row = v.sections.single().rows.single()
        assertEquals("claude · 40 calls", row.name); assertEquals("3.8M", row.tokens); assertEquals("≈ $4.50", row.usd)
        assertEquals(listOf("Root resets: 2"), v.notes)
        assertEquals(listOf("24h", "7d", "30d"), COST_WINDOWS.map { it.first })
    }

    @Test fun `an agent that has done nothing has no age, whatever the clocks say`() {
        val now = 1_000_000_000L
        val idle = agent("w1")
        // The host's clock is a second behind the phone's: nothing must turn into a date long ago.
        assertEquals("", activityAgo(idle, skewMs = -1_000, now = now))
        assertEquals("", activityAgo(idle, skewMs = 1_000, now = now))
        val busy = idle.toBuilder().setLastActivity(ts(now - 12_000)).build()
        assertEquals("12 s", activityAgo(busy, skewMs = 0, now = now))
        assertEquals("", sentAgo(message("m1", from = "w1", to = "lead", type = MessageType.MESSAGE_TYPE_INFO).toBuilder().clearCreatedAt().build(), 0, now))
    }

    @Test fun `the dock of every host merges the tiles, the ones that need you first, then the latest activity on the phone's clock`() {
        fun ag(id: String, st: AgentState, at: Long, name: String = id) = agent(id, state = st, name = name).toBuilder().setLastActivity(ts(at)).build()
        val home = DockHost("h1", "Home", skewMs = 0, runs = mapOf("r1" to run("r1")), agents = listOf(ag("a", AgentState.AGENT_STATE_WORKING, 10_000), ag("b", AgentState.AGENT_STATE_WORKING, 5_000)).associateBy { it.id }, messages = emptyMap())
        // The other host's clock is an hour ahead: its 10_000 + 3_600_000 is 10_000 here, and "d" asks for you.
        val work = DockHost("h2", "Work", skewMs = 3_600_000, runs = mapOf("r1" to run("r1")), agents = listOf(ag("c", AgentState.AGENT_STATE_WORKING, 3_600_000 + 7_000), ag("d", AgentState.AGENT_STATE_NEEDS_INPUT, 3_600_000 + 1_000)).associateBy { it.id }, messages = emptyMap())
        val tiles = dockTilesAcross(listOf(home, work))
        assertEquals(listOf("h2/d", "h1/a", "h2/c", "h1/b"), tiles.map { it.hostId + "/" + it.agent.id })
        assertEquals(listOf("Work", "Home", "Work", "Home"), tiles.map { it.hostName })
        assertEquals(3_600_000L, tiles[0].skewMs)
        // One host alone gives what its own dock gives.
        assertEquals(dockTiles(home.runs, home.agents, home.messages).map { it.agent.id }, dockTilesAcross(listOf(home)).map { it.agent.id })
    }

    @Test fun `agents with no activity sort by name whatever their host's clock says`() {
        fun ag(id: String) = agent(id, state = AgentState.AGENT_STATE_WORKING, name = id)
        val a = DockHost("h1", "Home", skewMs = 0, runs = mapOf("r1" to run("r1")), agents = mapOf("z" to ag("z")), messages = emptyMap())
        val b = DockHost("h2", "Work", skewMs = 3_600_000, runs = mapOf("r1" to run("r1")), agents = mapOf("a" to ag("a")), messages = emptyMap())
        assertEquals(listOf("a", "z"), dockTilesAcross(listOf(a, b)).map { it.agent.name })
    }
}
