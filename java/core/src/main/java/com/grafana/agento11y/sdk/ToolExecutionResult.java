package com.grafana.agento11y.sdk;

import java.time.Instant;

/** Completion payload for tool execution spans. */
public final class ToolExecutionResult {
    private Object arguments;
    private Object result;
    private Instant completedAt;
    private Long processExitCode;
    private String verifiedSkillName = "";
    private String verifiedSkillResourceName = "";
    private Boolean processExitCodeIsError;

    public Object getArguments() {
        return arguments;
    }

    public ToolExecutionResult setArguments(Object arguments) {
        this.arguments = arguments;
        return this;
    }

    public Object getResult() {
        return result;
    }

    public ToolExecutionResult setResult(Object result) {
        this.result = result;
        return this;
    }

    public Instant getCompletedAt() {
        return completedAt;
    }

    public ToolExecutionResult setCompletedAt(Instant completedAt) {
        this.completedAt = completedAt;
        return this;
    }

    public Long getProcessExitCode() {
        return processExitCode;
    }

    public ToolExecutionResult setProcessExitCode(Long processExitCode) {
        this.processExitCode = processExitCode;
        return this;
    }

    /** Caller-confirmed identity for metrics; never inferred from attempted identity. */
    public String getVerifiedSkillName() {
        return verifiedSkillName;
    }

    public ToolExecutionResult setVerifiedSkillName(String verifiedSkillName) {
        this.verifiedSkillName = verifiedSkillName == null ? "" : verifiedSkillName;
        return this;
    }

    /** Caller-confirmed resource identity for resource-read and command metrics. */
    public String getVerifiedSkillResourceName() {
        return verifiedSkillResourceName;
    }

    public ToolExecutionResult setVerifiedSkillResourceName(String verifiedSkillResourceName) {
        this.verifiedSkillResourceName = verifiedSkillResourceName == null ? "" : verifiedSkillResourceName;
        return this;
    }

    /** False marks a nonzero command exit as benign; does not suppress explicit execution errors. */
    public Boolean getProcessExitCodeIsError() {
        return processExitCodeIsError;
    }

    public ToolExecutionResult setProcessExitCodeIsError(Boolean processExitCodeIsError) {
        this.processExitCodeIsError = processExitCodeIsError;
        return this;
    }

    public ToolExecutionResult copy() {
        return new ToolExecutionResult()
                .setArguments(arguments)
                .setResult(result)
                .setCompletedAt(completedAt)
                .setProcessExitCode(processExitCode)
                .setVerifiedSkillName(verifiedSkillName)
                .setVerifiedSkillResourceName(verifiedSkillResourceName)
                .setProcessExitCodeIsError(processExitCodeIsError);
    }
}
