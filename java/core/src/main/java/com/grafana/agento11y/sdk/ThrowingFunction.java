package com.grafana.agento11y.sdk;

@FunctionalInterface
public interface ThrowingFunction<T, R> {
    R apply(T value) throws Exception;
}
