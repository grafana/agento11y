namespace Grafana.Sigil;

public static class GenerationValidator
{
    public static void Validate(Generation generation)
    {
        var contentStripped = SigilClient.IsContentStripped(generation);

        if (generation.Mode is not null && generation.Mode is not (GenerationMode.Sync or GenerationMode.Stream))
        {
            throw new ArgumentException("generation.mode must be one of SYNC|STREAM");
        }

        if (string.IsNullOrWhiteSpace(generation.Model.Provider))
        {
            throw new ArgumentException("generation.model.provider is required");
        }

        if (string.IsNullOrWhiteSpace(generation.Model.Name))
        {
            throw new ArgumentException("generation.model.name is required");
        }

        for (var i = 0; i < generation.Input.Count; i++)
        {
            ValidateMessage("generation.input", i, generation.Input[i], contentStripped);
        }

        for (var i = 0; i < generation.Output.Count; i++)
        {
            ValidateMessage("generation.output", i, generation.Output[i], contentStripped);
        }

        for (var i = 0; i < generation.Tools.Count; i++)
        {
            if (string.IsNullOrWhiteSpace(generation.Tools[i].Name))
            {
                throw new ArgumentException($"generation.tools[{i}].name is required");
            }
        }

        for (var i = 0; i < generation.Artifacts.Count; i++)
        {
            ValidateArtifact(i, generation.Artifacts[i]);
        }
    }

    private static void ValidateMessage(string path, int messageIndex, Message message, bool contentStripped)
    {
        if (message.Role is not (MessageRole.User or MessageRole.Assistant or MessageRole.Tool))
        {
            throw new ArgumentException($"{path}[{messageIndex}].role must be one of user|assistant|tool");
        }

        if (message.Parts.Count == 0)
        {
            throw new ArgumentException($"{path}[{messageIndex}].parts must not be empty");
        }

        for (var i = 0; i < message.Parts.Count; i++)
        {
            ValidatePart(path, messageIndex, i, message.Role, message.Parts[i], contentStripped);
        }
    }

    private static void ValidatePart(string path, int messageIndex, int partIndex, MessageRole role, Part part, bool contentStripped)
    {
        if (part.Kind is not (PartKind.Text or PartKind.Thinking or PartKind.ToolCall or PartKind.ToolResult))
        {
            throw new ArgumentException($"{path}[{messageIndex}].parts[{partIndex}].kind is invalid");
        }

        var fieldCount = 0;
        if (!string.IsNullOrWhiteSpace(part.Text))
        {
            fieldCount++;
        }

        if (!string.IsNullOrWhiteSpace(part.Thinking))
        {
            fieldCount++;
        }

        if (part.ToolCall != null)
        {
            fieldCount++;
        }

        if (part.ToolResult != null)
        {
            fieldCount++;
        }

        // Stripped text/thinking parts have empty payloads — that's expected.
        var strippedTextOrThinking = contentStripped && part.Kind is PartKind.Text or PartKind.Thinking;
        if (fieldCount != 1 && !strippedTextOrThinking)
        {
            throw new ArgumentException($"{path}[{messageIndex}].parts[{partIndex}] must set exactly one payload field");
        }

        switch (part.Kind)
        {
            case PartKind.Text when !contentStripped && string.IsNullOrWhiteSpace(part.Text):
                throw new ArgumentException($"{path}[{messageIndex}].parts[{partIndex}].text is required");
            case PartKind.Thinking:
                if (role != MessageRole.Assistant)
                {
                    throw new ArgumentException(
                        $"{path}[{messageIndex}].parts[{partIndex}].thinking only allowed for assistant role"
                    );
                }

                if (!contentStripped && string.IsNullOrWhiteSpace(part.Thinking))
                {
                    throw new ArgumentException($"{path}[{messageIndex}].parts[{partIndex}].thinking is required");
                }

                break;
            case PartKind.ToolCall:
                if (role != MessageRole.Assistant)
                {
                    throw new ArgumentException(
                        $"{path}[{messageIndex}].parts[{partIndex}].tool_call only allowed for assistant role"
                    );
                }

                if (part.ToolCall == null || string.IsNullOrWhiteSpace(part.ToolCall.Name))
                {
                    throw new ArgumentException($"{path}[{messageIndex}].parts[{partIndex}].tool_call.name is required");
                }

                break;
            case PartKind.ToolResult:
                if (role != MessageRole.Tool)
                {
                    throw new ArgumentException(
                        $"{path}[{messageIndex}].parts[{partIndex}].tool_result only allowed for tool role"
                    );
                }

                if (part.ToolResult == null)
                {
                    throw new ArgumentException($"{path}[{messageIndex}].parts[{partIndex}].tool_result is required");
                }

                break;
        }
    }

    private static void ValidateArtifact(int index, Artifact artifact)
    {
        if (artifact.Kind is not (
                ArtifactKind.Request
                or ArtifactKind.Response
                or ArtifactKind.Tools
                or ArtifactKind.ProviderEvent
            ))
        {
            throw new ArgumentException($"generation.artifacts[{index}].kind is invalid");
        }

        if (string.IsNullOrWhiteSpace(artifact.RecordId) && artifact.Payload.Length == 0)
        {
            throw new ArgumentException($"generation.artifacts[{index}] must provide payload or record_id");
        }
    }

    public static void ValidateEmbeddingStart(EmbeddingStart start)
    {
        var value = start ?? new EmbeddingStart();
        if (string.IsNullOrWhiteSpace(value.Model.Provider))
        {
            throw new ArgumentException("embedding.model.provider is required");
        }

        if (string.IsNullOrWhiteSpace(value.Model.Name))
        {
            throw new ArgumentException("embedding.model.name is required");
        }

        if (value.Dimensions.HasValue && value.Dimensions.Value <= 0)
        {
            throw new ArgumentException("embedding.dimensions must be > 0");
        }

        if (value.EncodingFormat.Length > 0 && string.IsNullOrWhiteSpace(value.EncodingFormat))
        {
            throw new ArgumentException("embedding.encoding_format must not be blank");
        }
    }

    public static void ValidateEmbeddingResult(EmbeddingResult result)
    {
        var value = result ?? new EmbeddingResult();
        if (value.InputCount < 0)
        {
            throw new ArgumentException("embedding.input_count must be >= 0");
        }

        if (value.InputTokens < 0)
        {
            throw new ArgumentException("embedding.input_tokens must be >= 0");
        }

        if (value.Dimensions.HasValue && value.Dimensions.Value <= 0)
        {
            throw new ArgumentException("embedding.dimensions must be > 0");
        }
    }
}
