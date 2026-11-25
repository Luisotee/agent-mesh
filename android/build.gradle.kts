plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.android.library) apply false
    alias(libs.plugins.kotlin.compose) apply false
    alias(libs.plugins.kotlin.serialization) apply false
}

// What code generation (scripts/gen.sh) needs, resolved and checksum-verified by Gradle so nothing is installed:
// the classpath of buf's connect-kotlin plugin, and protoc (which buf's built-in Java generator runs).
val codegen by configurations.creating
val protocExe by configurations.creating
val protocVersion = libs.versions.protobuf.get()
val protocClassifier = if (System.getProperty("os.arch") in setOf("aarch64", "arm64")) "linux-aarch_64" else "linux-x86_64"
dependencies {
    codegen(libs.connect.plugin)
    protocExe("com.google.protobuf:protoc:$protocVersion:$protocClassifier@exe")
}
tasks.register("writeCodegenTools") {
    val classpath = codegen.incoming.files
    val protoc = protocExe.incoming.files
    val dir = layout.buildDirectory.dir("codegen")
    outputs.dir(dir)
    doLast {
        val out = dir.get().asFile.apply { mkdirs() }
        File(out, "classpath.txt").writeText(classpath.files.joinToString(":") { it.absolutePath })
        protoc.singleFile.copyTo(File(out, "protoc"), overwrite = true).setExecutable(true)
    }
}
