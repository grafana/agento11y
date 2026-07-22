plugins {
    alias(libs.plugins.jmh) apply false
    alias(libs.plugins.protobuf) apply false
    alias(libs.plugins.maven.publish) apply false
}

allprojects {
    group = "com.grafana.agento11y"
    version = findProperty("version")?.toString() ?: "0.1.0-SNAPSHOT"
}

subprojects {
    plugins.withId("java") {
        extensions.configure<JavaPluginExtension> {
            toolchain {
                languageVersion.set(JavaLanguageVersion.of(17))
            }
        }

        tasks.withType<Jar>().configureEach {
            manifest {
                attributes["Implementation-Title"] = project.name
                attributes["Implementation-Version"] = project.version
            }
        }

        tasks.withType<Test>().configureEach {
            useJUnitPlatform()
            // Clear canonical AGENTO11Y_* and legacy SIGIL_* env vars so
            // individual tests don't pick up developer-machine config when
            // they construct a Agento11yClient. Tests that exercise env layering
            // should pass an explicit lookup to Agento11yEnvConfig.resolveFromEnv.
            listOf(
                "ENDPOINT", "PROTOCOL", "INSECURE", "HEADERS",
                "AUTH_MODE", "AUTH_TENANT_ID", "AUTH_TOKEN",
                "AGENT_NAME", "AGENT_VERSION", "USER_ID", "TAGS",
                "CONTENT_CAPTURE_MODE", "DEBUG",
            ).forEach { suffix ->
                environment("AGENTO11Y_$suffix", "")
                environment("SIGIL_$suffix", "")
            }
        }
    }

    plugins.withId("com.vanniktech.maven.publish") {
        extensions.configure<com.vanniktech.maven.publish.MavenPublishBaseExtension> {
            publishToMavenCentral(com.vanniktech.maven.publish.SonatypeHost.CENTRAL_PORTAL)
            signAllPublications()

            pom {
                name.set(project.name)
                description.set("Grafana Agent Observability SDK for Java - ${project.name}")
                url.set("https://github.com/grafana/agento11y")
                inceptionYear.set("2025")

                licenses {
                    license {
                        name.set("Apache License, Version 2.0")
                        url.set("https://www.apache.org/licenses/LICENSE-2.0.txt")
                        distribution.set("repo")
                    }
                }

                developers {
                    developer {
                        id.set("grafana")
                        name.set("Grafana Labs")
                        url.set("https://grafana.com")
                    }
                }

                scm {
                    url.set("https://github.com/grafana/agento11y")
                    connection.set("scm:git:git://github.com/grafana/agento11y.git")
                    developerConnection.set("scm:git:ssh://git@github.com/grafana/agento11y.git")
                }
            }
        }
    }
}
