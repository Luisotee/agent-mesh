package dev.agentmesh.android

import com.google.protobuf.Value
import dev.agentmesh.android.net.toJson
import dev.agentmesh.android.net.toValue
import dev.agentmesh.android.ui.settings.Control
import dev.agentmesh.android.ui.settings.Parsed
import dev.agentmesh.android.ui.settings.controlFor
import dev.agentmesh.android.ui.settings.fromText
import dev.agentmesh.android.ui.settings.groupOf
import dev.agentmesh.android.ui.settings.toText
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonPrimitive
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class SettingsFormTest {
    @Test fun `a control comes from the schema type alone`() {
        assertEquals(Control.Bool, controlFor("bool", emptyList()))
        assertEquals(Control.Number(true, 0.0, 100.0), controlFor("percent", emptyList()))
        assertEquals(Control.Select(listOf("low", "medium", "high", "xhigh", "max")), controlFor("effort", emptyList()))
        assertEquals(Control.Multi(listOf("claude", "agy")), controlFor("kinds", listOf("claude", "agy")))
        assertEquals(Control.Lines, controlFor("chain", emptyList()))
        assertEquals(Control.RawJson, controlFor("a-type-a-newer-host-has", emptyList()))
    }

    @Test fun `text round trips through a field`() {
        val lines = Control.Lines
        assertEquals(JsonArray(listOf(JsonPrimitive("a"), JsonPrimitive("b"))), (fromText("chain", lines, " a \n\nb\n") as Parsed.Ok).value)
        assertEquals("a\nb", toText(lines, JsonArray(listOf(JsonPrimitive("a"), JsonPrimitive("b")))))
        assertEquals(JsonPrimitive(75L), (fromText("percent", Control.Number(true, 0.0, 100.0), "75") as Parsed.Ok).value)
        assertEquals("75", toText(Control.Number(true, 0.0), JsonPrimitive(75.0)))
        assertEquals(JsonPrimitive(2048L), (fromText("mb_or_auto", Control.Text, "2048") as Parsed.Ok).value)
        assertEquals(JsonPrimitive("auto"), (fromText("mb_or_auto", Control.Text, " auto ") as Parsed.Ok).value)
    }

    @Test fun `only what cannot be sent is refused here`() {
        assertTrue(fromText("int", Control.Number(true, 0.0), "") is Parsed.Bad)
        assertTrue(fromText("int", Control.Number(true, 0.0), "x") is Parsed.Bad)
        assertTrue(fromText("json", Control.RawJson, "{nope") is Parsed.Bad)
        // -3 is the host's to refuse, with its own message.
        assertTrue(fromText("int", Control.Number(true, 0.0), "-3") is Parsed.Ok)
    }

    @Test fun `groups are the first key segment`() {
        assertEquals("tiers", groupOf("tiers.claude.mid.model"))
        assertEquals("general", groupOf("quota_stop"))
    }

    @Test fun `protobuf values and JSON convert both ways`() {
        val json = Json.parseToJsonElement("""{"a":[1,"x",true,null],"b":{"c":2.5}}""")
        val value: Value = json.toValue()
        assertEquals(json, value.toJson())
        assertEquals(JsonPrimitive(3), JsonPrimitive(3).toValue().toJson())
    }
}
