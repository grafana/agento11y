package com.grafana.agento11y.sdk;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/** Debug/test snapshot of in-memory runtime state. */
public final class Agento11yDebugSnapshot {
    private final List<Generation> generations;
    private final List<ToolExecution> toolExecutions;
    private final int queueSize;

    public Agento11yDebugSnapshot(List<Generation> generations, List<ToolExecution> toolExecutions, int queueSize) {
        List<Generation> generationCopies = new ArrayList<>();
        if (generations != null) {
            for (Generation generation : generations) {
                generationCopies.add(generation == null ? new Generation() : generation.copy());
            }
        }
        List<ToolExecution> executionCopies = new ArrayList<>();
        if (toolExecutions != null) {
            for (ToolExecution toolExecution : toolExecutions) {
                executionCopies.add(toolExecution == null ? new ToolExecution() : toolExecution.copy());
            }
        }

        this.generations = Collections.unmodifiableList(generationCopies);
        this.toolExecutions = Collections.unmodifiableList(executionCopies);
        this.queueSize = queueSize;
    }

    public List<Generation> getGenerations() {
        return generations;
    }

    public List<ToolExecution> getToolExecutions() {
        return toolExecutions;
    }

    public int getQueueSize() {
        return queueSize;
    }
}
