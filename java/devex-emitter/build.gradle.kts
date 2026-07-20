plugins {
    application
}

dependencies {
    implementation(project(":core"))
    implementation(project(":providers:openai"))
    implementation(project(":providers:anthropic"))
    implementation(project(":providers:gemini"))
    implementation(libs.anthropic.java)
    implementation(libs.google.genai)
    implementation(libs.openai.java)

    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
    testImplementation(libs.assertj.core)
}

application {
    mainClass.set("com.grafana.agento11y.sdk.devex.DevexEmitter")
}
