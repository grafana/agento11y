rootProject.name = "agento11y-java"

include(":core")
include(":providers:openai")
include(":providers:anthropic")
include(":providers:gemini")
include(":frameworks:google-adk")
include(":benchmarks")
include(":devex-emitter")

project(":providers:openai").projectDir = file("providers/openai")
project(":providers:anthropic").projectDir = file("providers/anthropic")
project(":providers:gemini").projectDir = file("providers/gemini")
project(":frameworks:google-adk").projectDir = file("frameworks/google-adk")

pluginManagement {
    repositories {
        gradlePluginPortal()
        mavenCentral()
    }
}

plugins {
    id("org.gradle.toolchains.foojay-resolver-convention") version "1.0.0"
}

dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        mavenCentral()
    }
}
