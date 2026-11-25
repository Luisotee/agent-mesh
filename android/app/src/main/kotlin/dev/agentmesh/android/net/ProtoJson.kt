package dev.agentmesh.android.net

import com.google.protobuf.ListValue
import com.google.protobuf.NullValue
import com.google.protobuf.Struct
import com.google.protobuf.Value
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.doubleOrNull

/** `google.protobuf.Value` is how settings travel; the protobuf-lite runtime has no JSON mapping, so this is it. */
fun Value.toJson(): JsonElement = when (kindCase) {
    Value.KindCase.NULL_VALUE, Value.KindCase.KIND_NOT_SET -> JsonNull
    // A whole number reads back as one (settings are mostly integers), not as 75.0.
    Value.KindCase.NUMBER_VALUE -> if (numberValue == Math.rint(numberValue) && Math.abs(numberValue) < 1e15) JsonPrimitive(numberValue.toLong()) else JsonPrimitive(numberValue)
    Value.KindCase.STRING_VALUE -> JsonPrimitive(stringValue)
    Value.KindCase.BOOL_VALUE -> JsonPrimitive(boolValue)
    Value.KindCase.STRUCT_VALUE -> JsonObject(structValue.fieldsMap.mapValues { it.value.toJson() })
    Value.KindCase.LIST_VALUE -> JsonArray(listValue.valuesList.map { it.toJson() })
}

fun JsonElement.toValue(): Value = when (this) {
    is JsonNull -> Value.newBuilder().setNullValue(NullValue.NULL_VALUE).build()
    is JsonObject -> Value.newBuilder().setStructValue(Struct.newBuilder().putAllFields(mapValues { it.value.toValue() })).build()
    is JsonArray -> Value.newBuilder().setListValue(ListValue.newBuilder().addAllValues(map { it.toValue() })).build()
    is JsonPrimitive -> when {
        isString -> Value.newBuilder().setStringValue(content).build()
        booleanOrNull != null -> Value.newBuilder().setBoolValue(booleanOrNull!!).build()
        else -> Value.newBuilder().setNumberValue(doubleOrNull ?: error("not a number: $content")).build()
    }
}
