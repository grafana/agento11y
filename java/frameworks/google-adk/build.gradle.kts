plugins {
    `java-library`
    alias(libs.plugins.maven.publish)
}

mavenPublishing {
    coordinates(artifactId = "agento11y-google-adk")
}

dependencies {
    api(project(":core"))

    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
    testImplementation(libs.assertj.core)
    testImplementation(libs.otel.sdk.trace)
    testImplementation(libs.otel.sdk.metrics)
    testImplementation(libs.otel.sdk.testing)
}
