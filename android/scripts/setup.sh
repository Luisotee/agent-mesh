#!/usr/bin/env bash
# Installs what the Android build needs under ~/.local/opt, no sudo, nothing global:
#   android-sdk/ (command-line tools, platform-tools, one platform, build-tools), jdk-21/ and gradle-<version>/ (only to
#   bootstrap the wrapper; the project builds with ./gradlew). Safe to re-run. Prints the env scripts/env.sh sets.
set -euo pipefail
OPT="${AGENT_MESH_OPT:-$HOME/.local/opt}"
SDK="$OPT/android-sdk"
CMDLINE_REV=16111833
CMDLINE_SHA1=e025545c62a8e64c7559119566a569fb1dec5f60
PLATFORM=android-37.2
BUILD_TOOLS=37.0.0
GRADLE_VERSION=9.9.2
JDK_URL="https://github.com/adoptium/temurin21-binaries/releases/download/jdk-21.0.13.1%2B2/OpenJDK21U-jdk_x64_linux_hotspot_21.0.13.1_2.tar.gz"
JDK_SHA256=bb8a93f9ae6e7a693b2b721e85dbeff26ae1a77447985a8d7afcdd9c7cf6222e
JDK_DIR="$OPT/jdk-21"

mkdir -p "$SDK" "$OPT/dl"
if [ ! -x "$SDK/cmdline-tools/latest/bin/sdkmanager" ]; then
  zip="$OPT/dl/commandlinetools-linux-${CMDLINE_REV}_latest.zip"
  [ -f "$zip" ] || curl -fL -o "$zip" "https://dl.google.com/android/repository/commandlinetools-linux-${CMDLINE_REV}_latest.zip"
  echo "$CMDLINE_SHA1  $zip" | sha1sum -c -
  rm -rf "$SDK/cmdline-tools" "$OPT/dl/cmdline-tools"
  unzip -q "$zip" -d "$OPT/dl"
  mkdir -p "$SDK/cmdline-tools"
  mv "$OPT/dl/cmdline-tools" "$SDK/cmdline-tools/latest"
fi
yes | "$SDK/cmdline-tools/latest/bin/sdkmanager" --sdk_root="$SDK" --licenses >/dev/null || true
"$SDK/cmdline-tools/latest/bin/sdkmanager" --sdk_root="$SDK" "platform-tools" "platforms;$PLATFORM" "build-tools;$BUILD_TOOLS"

if [ ! -x "$OPT/gradle-$GRADLE_VERSION/bin/gradle" ]; then
  zip="$OPT/dl/gradle-$GRADLE_VERSION-bin.zip"
  [ -f "$zip" ] || curl -fL -o "$zip" "https://services.gradle.org/distributions/gradle-$GRADLE_VERSION-bin.zip"
  curl -fsSL "https://services.gradle.org/distributions/gradle-$GRADLE_VERSION-bin.zip.sha256" | awk -v f="$zip" '{print $1"  "f}' | sha256sum -c -
  unzip -q "$zip" -d "$OPT"
fi
# The build needs a JDK (javac compiles the generated protobuf Java); the machine may have only a JRE.
if [ ! -x "$JDK_DIR/bin/javac" ]; then
  tgz="$OPT/dl/$(basename "${JDK_URL//%2B/+}")"
  [ -f "$tgz" ] || curl -fL -o "$tgz" "$JDK_URL"
  echo "$JDK_SHA256  $tgz" | sha256sum -c -
  rm -rf "$JDK_DIR" && mkdir -p "$JDK_DIR" && tar -xzf "$tgz" -C "$JDK_DIR" --strip-components=1
fi
echo "JDK: $JDK_DIR"
echo "Android SDK: $SDK"
echo "Gradle: $OPT/gradle-$GRADLE_VERSION"
