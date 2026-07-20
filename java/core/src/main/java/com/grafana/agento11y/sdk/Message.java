package com.grafana.agento11y.sdk;

import java.util.ArrayList;
import java.util.List;

/** Normalized generation message. */
public final class Message {
    private MessageRole role = MessageRole.USER;
    private String name = "";
    private final List<MessagePart> parts = new ArrayList<>();

    public MessageRole getRole() {
        return role;
    }

    public Message setRole(MessageRole role) {
        this.role = role == null ? MessageRole.USER : role;
        return this;
    }

    public String getName() {
        return name;
    }

    public Message setName(String name) {
        this.name = name == null ? "" : name;
        return this;
    }

    public List<MessagePart> getParts() {
        return parts;
    }

    public Message setParts(List<MessagePart> parts) {
        this.parts.clear();
        if (parts != null) {
            for (MessagePart part : parts) {
                this.parts.add(part == null ? new MessagePart() : part);
            }
        }
        return this;
    }

    public Message copy() {
        Message out = new Message().setRole(role).setName(name);
        for (MessagePart part : parts) {
            out.getParts().add(part == null ? new MessagePart() : part.copy());
        }
        return out;
    }
}
