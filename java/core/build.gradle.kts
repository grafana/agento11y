plugins {
    `java-library`
    alias(libs.plugins.protobuf)
    alias(libs.plugins.maven.publish)
}

mavenPublishing {
    coordinates(artifactId = "sigil-sdk-core")
}

val protoRoot = rootProject.projectDir.resolve("../proto")

dependencies {
    api(libs.otel.api)
    api(libs.otel.context)

    implementation(libs.jackson.core)
    implementation(libs.jackson.databind)
    implementation(libs.jackson.datatype.jsr310)

    implementation(libs.protobuf.java)
    implementation(libs.protobuf.java.util)
    compileOnly(libs.javax.annotation)

    implementation(libs.grpc.netty)
    implementation(libs.grpc.protobuf)
    implementation(libs.grpc.stub)

    implementation(libs.otel.sdk.trace)
    implementation(libs.otel.sdk.metrics)
    implementation(libs.otel.exporter.otlp)

    testImplementation(platform(libs.junit.bom))
    testImplementation(libs.junit.jupiter)
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
    testImplementation(libs.assertj.core)
    testImplementation(libs.mockwebserver)
    testImplementation("io.opentelemetry.proto:opentelemetry-proto:1.9.0-alpha")
    testImplementation(libs.otel.sdk.testing)
}

sourceSets {
    main {
        proto {
            srcDir(protoRoot)
        }
    }
}

protobuf {
    protoc {
        artifact = "com.google.protobuf:protoc:${libs.versions.protobuf.get()}"
    }
    plugins {
        create("grpc") {
            artifact = "io.grpc:protoc-gen-grpc-java:${libs.versions.grpc.get()}"
        }
    }
    generateProtoTasks {
        all().configureEach {
            plugins {
                create("grpc")
            }
        }
    }
}
