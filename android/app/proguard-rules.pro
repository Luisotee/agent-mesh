# The release-type APK (assembleRelease, signed with the debug key): R8 only optimizes (inlining, devirtualization, Compose's
# own call-site work), which is what makes the UI faster than the debug build. It does not shrink or rename, so nothing that
# is reached by reflection or by name can go missing: the generated protobuf and ConnectRPC code (reflection on field and
# accessor names), the terminal emulator, the Markdown parser (element types looked up by name).
-dontobfuscate
-dontshrink

-keep class dev.agentmesh.gen.** { *; }
-keep class com.google.protobuf.** { *; }
-keep class com.connectrpc.** { *; }
-keep class com.termux.terminal.** { *; }
-keep class org.intellij.markdown.** { *; }
-keep class kotlinx.serialization.** { *; }
-keepclassmembers class **$$serializer { *; }
-keepclasseswithmembers class * { @kotlinx.serialization.Serializable <fields>; }

# Optional classes the libraries mention and the app does not ship (the OEM window extensions, the JVM management beans).
-dontwarn androidx.window.**
-dontwarn java.lang.management.**
-dontwarn org.slf4j.**
-dontwarn javax.annotation.**
-dontwarn com.google.errorprone.annotations.**
-dontwarn org.conscrypt.**
-dontwarn org.bouncycastle.**
-dontwarn org.openjsse.**
