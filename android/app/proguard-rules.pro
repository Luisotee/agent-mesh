# The release-type APK (assembleRelease, signed with the debug key): not debuggable, with the libraries' baseline profiles compiled in,
# and run through R8 without shrinking or renaming (so how much R8 optimizes under -dontshrink is not measured, and the APK is not
# small). Nothing that is reached by reflection or by name can go missing: the generated protobuf and ConnectRPC code (reflection
# on field and accessor names), the terminal emulator, the Markdown parser (element types looked up by name).
-dontobfuscate
-dontshrink

-keep class dev.agentmesh.gen.** { *; }
-keep class com.google.protobuf.** { *; }
-keep class com.connectrpc.** { *; }
-keep class com.termux.terminal.** { *; }
-keep class org.intellij.markdown.** { *; }
-keep class kotlinx.serialization.** { *; }
-keepclassmembers class **$$serializer { *; }
-keep @kotlinx.serialization.Serializable class * { *; }

# Optional classes the libraries mention and the app does not ship (the OEM window extensions, the JVM management beans).
-dontwarn androidx.window.**
-dontwarn java.lang.management.**
-dontwarn org.slf4j.**
-dontwarn javax.annotation.**
-dontwarn com.google.errorprone.annotations.**
-dontwarn org.conscrypt.**
-dontwarn org.bouncycastle.**
-dontwarn org.openjsse.**
