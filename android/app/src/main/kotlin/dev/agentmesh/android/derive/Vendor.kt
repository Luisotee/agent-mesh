package dev.agentmesh.android.derive

/**
 * Which vendor a CLI or a model belongs to, for the icon beside it: the desktop's `vendor.ts`, case for case
 * (`VendorTest`). A CLI is known by its kind; a model by its name, since OpenCode and Copilot run other vendors' models.
 */
enum class Vendor(val label: String) {
    Claude("Anthropic Claude"), Gemini("Google Gemini"), OpenAI("OpenAI"), OpenCode("OpenCode"), Copilot("GitHub Copilot"),
    DeepSeek("DeepSeek"), Other("Unknown vendor"),
}

private val KIND = mapOf("claude" to Vendor.Claude, "agy" to Vendor.Gemini, "opencode" to Vendor.OpenCode, "copilot" to Vendor.Copilot, "codex" to Vendor.OpenAI)

fun vendorOfKind(kind: String): Vendor = KIND[kind.trim().lowercase()] ?: Vendor.Other

private val CLAUDE = Regex("(^|[^a-z])(claude|opus|sonnet|haiku|fable)([^a-z]|$)")
private val OPENAI = Regex("^(gpt|o\\d|codex|chatgpt)|[-/](gpt|codex)")
private val GEMINI = Regex("gemini|neon|carbon|argon|hydrogen|flash|(^|[^a-z])pro([^a-z]|$)")

/** The vendor of a model by its name or family (`claude-sonnet-5-5`, `opus`, `gemini-4-neon-high`, `gpt-5`), or Other. */
fun vendorOfModel(model: String): Vendor {
    val m = model.trim().lowercase()
    return when {
        m.isEmpty() -> Vendor.Other
        // DeepSeek first: its models are named `deepseek-v4-flash` and `-pro`, which the Gemini names below would claim.
        "deepseek" in m -> Vendor.DeepSeek
        CLAUDE.containsMatchIn(m) -> Vendor.Claude
        // OpenAI before Gemini: `gpt-5-pro` and `o3-pro` carry the bare `pro` the Gemini names match.
        OPENAI.containsMatchIn(m) -> Vendor.OpenAI
        GEMINI.containsMatchIn(m) -> Vendor.Gemini
        else -> Vendor.Other
    }
}

/** The vendor to show for an agent: its model's when the model says (OpenCode runs another vendor's model), else its CLI's. */
fun vendorOf(kind: String, model: String = ""): Vendor {
    val byKind = vendorOfKind(kind)
    if (byKind == Vendor.OpenCode || byKind == Vendor.Copilot || byKind == Vendor.Other) {
        val byModel = vendorOfModel(model)
        return if (byModel == Vendor.Other) byKind else byModel
    }
    return byKind
}
