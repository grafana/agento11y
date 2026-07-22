package com.grafana.agento11y.sdk;

/**
 * SDK version and User-Agent product token.
 *
 * <p>{@link #VERSION} is stamped into the default generation-export User-Agent (see {@link
 * #userAgent()}). Resolved from the jar manifest's {@code Implementation-Version};
 * {@code 0.0.0+unknown} when manifest metadata is unavailable (e.g. classes-directory runs).
 */
public final class SdkVersion {
    /** Version of the Agento11y Java SDK. */
    public static final String VERSION = resolveVersion();

    private static final String USER_AGENT_PRODUCT = "agento11y-sdk-java";

    private SdkVersion() {}

    private static String resolveVersion() {
        String version = SdkVersion.class.getPackage().getImplementationVersion();
        return (version == null || version.isBlank()) ? "0.0.0+unknown" : version;
    }

    /**
     * Returns the SDK's default generation-export User-Agent product token, {@code
     * agento11y-sdk-java/<VERSION>}.
     */
    public static String userAgent() {
        return USER_AGENT_PRODUCT + "/" + VERSION;
    }
}
