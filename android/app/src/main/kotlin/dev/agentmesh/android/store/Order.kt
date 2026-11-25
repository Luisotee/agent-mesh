package dev.agentmesh.android.store

import com.google.protobuf.Timestamp
import dev.agentmesh.gen.agentmesh.v1.Agent
import dev.agentmesh.gen.agentmesh.v1.Message

fun Timestamp.toMillis(): Long = seconds * 1000 + nanos / 1_000_000

private fun Message.nanos(): Long = if (hasCreatedAt()) createdAt.seconds * 1_000_000_000L + createdAt.nanos else 0L

/**
 * The host's order of messages: time to the nanosecond, then id as bytes (SQLite's order). Every client-side sort
 * and comparison of messages uses this one, so the cursor of a resumed load names the message the host put last.
 */
val MessageOrder: Comparator<Message> = Comparator { a, b ->
    val x = a.nanos()
    val y = b.nanos()
    when {
        x != y -> x.compareTo(y)
        a.id == b.id -> 0
        else -> a.id.compareTo(b.id)
    }
}

fun isBefore(a: Message, b: Message): Boolean = MessageOrder.compare(a, b) < 0

val Agent.createdMillis: Long get() = if (hasCreatedAt()) createdAt.toMillis() else 0L
