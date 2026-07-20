package com.grafana.agento11y.sdk;

/**
 * SDK version and User-Agent product token.
 *
 * <p>{@link #VERSION} is stamped into the default generation-export User-Agent (see {@link
 * #userAgent()}). Keep in sync with the gradle {@code version} on release.
 */
public final class SdkVersion {
    /** Released version of the Sigil Java SDK. */
    public static final String VERSION = "0.2.0";

    private static final String USER_AGENT_PRODUCT = "agento11y-sdk-java";

    private SdkVersion() {}

    /**
     * Returns the SDK's default generation-export User-Agent product token, {@code
     * agento11y-sdk-java/<VERSION>}.
     */
    public static String userAgent() {
        return USER_AGENT_PRODUCT + "/" + VERSION;
    }
}
