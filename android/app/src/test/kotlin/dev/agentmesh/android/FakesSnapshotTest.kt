package dev.agentmesh.android

import org.junit.Test
import org.junit.Assert.assertEquals

/** The fakes' recordings are read through snapshot(): counting a synchronizedList while another thread appends must not fail. */
class FakesSnapshotTest {
    @Test fun `reading a recording while another thread appends never fails`() {
        val list = java.util.Collections.synchronizedList(mutableListOf<String>())
        val writer = Thread { repeat(200_000) { list += "x" } }
        writer.start()
        // At least one read, even when the writer has finished before this thread was scheduled.
        do {
            list.snapshot().count { it == "x" }
        } while (writer.isAlive)
        writer.join()
        assertEquals(200_000, list.snapshot().count { it == "x" })
    }

    @Test fun `a snapshot is a copy`() {
        val list = java.util.Collections.synchronizedList(mutableListOf("a"))
        val copy = list.snapshot()
        list += "b"
        assertEquals(listOf("a"), copy)
    }
}
