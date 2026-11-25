# Sourced by the Android scripts: the user-level toolchain from scripts/setup.sh, nothing global.
OPT="${AGENT_MESH_OPT:-$HOME/.local/opt}"
export ANDROID_HOME="$OPT/android-sdk"
export ANDROID_SDK_ROOT="$ANDROID_HOME"
export GRADLE_USER_HOME="${GRADLE_USER_HOME:-$OPT/gradle-home}"
export PATH="$HOME/.local/go/bin:$OPT/buf/bin:$PATH"
[ -x "$OPT/jdk-21/bin/javac" ] && export JAVA_HOME="$OPT/jdk-21" && export PATH="$JAVA_HOME/bin:$PATH"
