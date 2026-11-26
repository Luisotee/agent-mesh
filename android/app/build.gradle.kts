plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
}

android {
    namespace = "dev.agentmesh.android"
    compileSdk { version = release(37) { minorApiLevel = 2 } }

    defaultConfig {
        applicationId = "dev.agentmesh.android"
        minSdk = 26
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"
    }

    buildTypes {
        debug { applicationIdSuffix = ".debug" }
        release { isMinifyEnabled = false }
    }
    buildFeatures { compose = true }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    sourceSets {
        // The generated protobuf and ConnectRPC code (scripts/gen.sh), committed like daemon/gen.
        getByName("main") {
            java.directories.add("src/gen/java")
            kotlin.directories.add("src/gen/kotlin")
            // The theme files both apps share (../shared/themes/*.json, read as assets/themes/*.json).
            assets.directories.add("../../shared")
        }
    }
    testOptions {
        unitTests {
            isIncludeAndroidResources = true
            isReturnDefaultValues = true
            // SKIP_E2E=1 skips the tests that build and run a real meshd; it must change the task's inputs, or a
            // cached run of the other kind would be reused.
            all { test -> test.inputs.property("skipE2e", providers.environmentVariable("SKIP_E2E").orElse("")) }
        }
    }
    lint { abortOnError = true; warningsAsErrors = false }
}


dependencies {
    implementation(project(":terminal-emulator"))
    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.ui.tooling.preview)
    implementation(libs.compose.foundation)
    implementation(libs.compose.material3)
    implementation(libs.compose.material.icons)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.navigation.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.lifecycle.process)
    implementation(libs.coroutines.android)
    implementation(libs.serialization.json)
    implementation(libs.okhttp)
    implementation(libs.connect.kotlin)
    implementation(libs.connect.okhttp)
    implementation(libs.connect.javalite)
    implementation(libs.protobuf.javalite)
    implementation(libs.markdown.m3)
    // The pairing screen's QR scanner: CameraX for the preview and frames, ZXing (pure Java, Apache-2.0) to decode them.
    implementation(libs.camerax.core)
    implementation(libs.camerax.camera2)
    implementation(libs.camerax.lifecycle)
    implementation(libs.camerax.view)
    implementation(libs.zxing.core)
    debugImplementation(libs.compose.ui.tooling)
    debugImplementation(libs.compose.ui.test.manifest)

    testImplementation(libs.junit)
    testImplementation(libs.coroutines.test)
    testImplementation(libs.okhttp.mockwebserver)
    testImplementation(libs.okhttp.tls)
    testImplementation(libs.robolectric)
    testImplementation(platform(libs.compose.bom))
    testImplementation(libs.compose.ui.test.junit4)
}
