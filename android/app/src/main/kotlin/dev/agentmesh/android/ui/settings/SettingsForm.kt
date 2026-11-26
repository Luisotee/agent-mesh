package dev.agentmesh.android.ui.settings

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.doubleOrNull

/** How a setting is edited, from its schema's type. Nothing here knows any setting by name (the desktop's `settings-form.ts`). */
sealed interface Control {
    data object Bool : Control
    data class Number(val integer: Boolean, val min: Double, val max: Double? = null) : Control
    data object Text : Control
    data class Select(val choices: List<String>) : Control

    /** One entry per line: a list of strings where order matters (a chain of models). */
    data object Lines : Control

    /** A set of the choices. */
    data class Multi(val choices: List<String>) : Control
    data object RawJson : Control
}

private val EFFORTS = listOf("low", "medium", "high", "xhigh", "max")

fun controlFor(type: String, choices: List<String>): Control = when (type) {
    "bool" -> Control.Bool
    "int" -> Control.Number(true, 0.0)
    "percent" -> Control.Number(true, 0.0, 100.0)
    "seconds" -> Control.Number(false, 0.0, 1e9)
    "effort" -> Control.Select(choices.ifEmpty { EFFORTS })
    "kinds" -> if (choices.isNotEmpty()) Control.Multi(choices) else Control.Lines
    "strings", "chain" -> Control.Lines
    // A string with choices is one of them (the host refuses any other).
    "string" -> if (choices.isNotEmpty()) Control.Select(choices) else Control.Text
    "mb_or_auto" -> Control.Text
    // slots, json, and any type a newer host has that this app doesn't know: raw JSON is always right.
    else -> Control.RawJson
}

private val pretty = Json { prettyPrint = true }

/** A value as the text of a field. */
fun toText(c: Control, v: JsonElement): String = when (c) {
    Control.Lines -> (v as? JsonArray)?.joinToString("\n") { (it as? JsonPrimitive)?.content.orEmpty() }.orEmpty()
    Control.RawJson -> pretty.encodeToString(JsonElement.serializer(), v)
    is Control.Number -> (v as? JsonPrimitive)?.doubleOrNull?.let { d -> if (d == Math.rint(d) && Math.abs(d) < 1e15) d.toLong().toString() else d.toString() }.orEmpty()
    Control.Text, is Control.Select -> (v as? JsonPrimitive)?.takeIf { it !is JsonNull }?.content.orEmpty()
    Control.Bool -> if (v.asBool()) "on" else "off"
    is Control.Multi -> v.asStrings().joinToString(", ").ifEmpty { "none" }
}

sealed interface Parsed {
    data class Ok(val value: JsonElement) : Parsed
    data class Bad(val error: String) : Parsed
}

/**
 * A field's text as the JSON value to send. Only what can't be sent at all is refused here (not a number, not
 * JSON); whether the value is allowed is the host's check, whose message the form shows.
 */
fun fromText(type: String, c: Control, text: String): Parsed = when (c) {
    is Control.Number -> {
        val n = text.trim().toDoubleOrNull()
        when {
            text.isBlank() -> Parsed.Bad("enter a number")
            n == null || !n.isFinite() -> Parsed.Bad("not a number")
            else -> Parsed.Ok(JsonPrimitive(if (c.integer && n == Math.rint(n)) n.toLong() else n))
        }
    }
    Control.Lines -> Parsed.Ok(JsonArray(text.lines().map { it.trim() }.filter { it.isNotEmpty() }.map(::JsonPrimitive)))
    Control.RawJson -> try {
        Parsed.Ok(Json.parseToJsonElement(text))
    } catch (e: Exception) {
        Parsed.Bad("not valid JSON: ${e.message?.lineSequence()?.firstOrNull()}")
    }
    Control.Text -> {
        val t = text.trim()
        // mb_or_auto is "auto" or a whole number of MB.
        if (type == "mb_or_auto" && t.isNotEmpty() && t != "auto" && t.toDoubleOrNull() != null) Parsed.Ok(JsonPrimitive(t.toDouble().toLong()))
        else Parsed.Ok(JsonPrimitive(if (type == "mb_or_auto") t else text))
    }
    else -> Parsed.Ok(JsonPrimitive(text))
}

/** The group a key is shown under: its first segment (`tiers`, `review`, `app`), or "general" for a lone word. */
fun groupOf(key: String): String = key.indexOf('.').let { if (it < 0) "general" else key.substring(0, it) }

fun JsonElement.asBool(): Boolean = (this as? JsonPrimitive)?.booleanOrNull ?: false
fun JsonElement.asStrings(): List<String> = (this as? JsonArray)?.mapNotNull { (it as? JsonPrimitive)?.content }.orEmpty()
