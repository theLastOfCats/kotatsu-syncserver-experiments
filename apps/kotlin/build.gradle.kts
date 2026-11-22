plugins {
    kotlin("jvm") version "2.4.21"
    kotlin("plugin.serialization") version "2.4.21"
    application
}

group = "org.kotatsu"
version = "1.0.0"

repositories {
    mavenCentral()
}

val serverEngine = providers.gradleProperty("serverEngine").orElse("netty").get()
require(serverEngine == "netty" || serverEngine == "cio") {
    "serverEngine must be netty or cio"
}
if (serverEngine == "cio") {
    layout.buildDirectory.set(layout.projectDirectory.dir("build/cio"))
}

kotlin {
    jvmToolchain(21)
    sourceSets.named("main") {
        kotlin.srcDir("src/$serverEngine/kotlin")
    }
}

application {
    applicationName = if (serverEngine == "cio") "kotatsu-kotlin-cio" else "kotatsu-kotlin"
    mainClass = "kotatsu.MainKt"
}

dependencies {
    val ktorVersion = "3.6.1"
    implementation("io.ktor:ktor-server-core-jvm:$ktorVersion")
    implementation("io.ktor:ktor-server-$serverEngine-jvm:$ktorVersion")
    implementation("io.ktor:ktor-server-content-negotiation-jvm:$ktorVersion")
    implementation("io.ktor:ktor-serialization-kotlinx-json-jvm:$ktorVersion")
    implementation("io.ktor:ktor-server-status-pages-jvm:$ktorVersion")
    implementation("io.ktor:ktor-server-compression-jvm:$ktorVersion")
    implementation("io.ktor:ktor-server-rate-limit-jvm:$ktorVersion")
    implementation("io.ktor:ktor-server-auto-head-response-jvm:$ktorVersion")
    implementation(platform("com.fasterxml.jackson:jackson-bom:2.23.0"))
    implementation("com.auth0:java-jwt:4.6.2")
    implementation("de.mkammerer:argon2-jvm:2.12")
    implementation("com.sun.mail:jakarta.mail:2.0.2")
    implementation("com.zaxxer:HikariCP:7.1.0")
    implementation("org.xerial:sqlite-jdbc:3.54.1.0")
    implementation("com.mysql:mysql-connector-j:26.10.0")
    runtimeOnly("ch.qos.logback:logback-classic:1.6.8")
}

if (serverEngine == "cio") {
    tasks.named<Sync>("installDist") {
        into(layout.projectDirectory.dir("../../build/bench/kotlin-cio"))
    }
}
