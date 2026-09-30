import java.util.Properties

// Clave SSH por defecto de los routers (la de fábrica): no va en el repo. Se
// toma de android/local.properties (ignorado por git), `notion.sshPassword=...`.
val localProps = Properties().apply {
    val f = rootProject.file("local.properties")
    if (f.exists()) f.inputStream().use { load(it) }
}
val sshDefaultPassword: String = localProps.getProperty("notion.sshPassword", "")

plugins {
    id("com.android.application")
    id("kotlin-android")
    // The Flutter Gradle Plugin must be applied after the Android and Kotlin Gradle plugins.
    id("dev.flutter.flutter-gradle-plugin")
}

android {
    namespace = "com.h3s.notion5g.notion5g_field"
    compileSdk = flutter.compileSdkVersion
    ndkVersion = flutter.ndkVersion

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_11
        targetCompatibility = JavaVersion.VERSION_11
    }

    kotlinOptions {
        jvmTarget = JavaVersion.VERSION_11.toString()
    }

    defaultConfig {
        // TODO: Specify your own unique Application ID (https://developer.android.com/studio/build/application-id.html).
        applicationId = "com.h3s.notion5g.notion5g_field"
        // You can update the following values to match your application needs.
        // For more information, see: https://flutter.dev/to/review-gradle-config.
        minSdk = flutter.minSdkVersion
        targetSdk = flutter.targetSdkVersion
        versionCode = flutter.versionCode
        versionName = flutter.versionName
        buildConfigField("String", "SSH_DEFAULT_PASSWORD", "\"" + sshDefaultPassword.replace("\\", "\\\\").replace("\"", "\\\"") + "\"")
    }

    buildFeatures {
        buildConfig = true
    }

    buildTypes {
        release {
            // TODO: Add your own signing config for the release build.
            // Signing with the debug keys for now, so `flutter run --release` works.
            signingConfig = signingConfigs.getByName("debug")
            // JSch carga sus algoritmos por nombre (Class.forName): reglas en
            // proguard-rules.pro para que R8 no los borre ni los renombre.
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }

    packaging {
        resources {
            // Clases de Java 9+ del jar multi-release de JSch: Android no las usa.
            excludes += setOf("META-INF/versions/**")
        }
    }
}

dependencies {
    // SSH para reiniciar los routers (contrato §6.2). Fork mantenido de JSch:
    // todavía acepta ssh-rsa y diffie-hellman-group1/14-sha1 si se habilitan,
    // que es lo que exige el dropbear viejo del Notion.
    implementation("com.github.mwiede:jsch:0.2.26")
}

flutter {
    source = "../.."
}
