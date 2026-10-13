# Modality token usage

All five SDKs support optional input, output, cache-read and cache-write modality
partitions. Each partition contains a token map and a `complete` flag. Preserve
provider counts; do not estimate media tokens from pixels, duration, or image count.
Input is inclusive of cache subsets. Output includes reasoning exactly once.

A complete partition accounts for the entire bucket, so absent modalities mean
zero. An incomplete or absent partition leaves the allocation unknown. Sigil
marks costs unavailable if differing rates require unknown detail. Missing cached
modality intersections cannot be reconstructed from aggregate cached tokens.

Native HTTP/gRPC payloads and the `agento11y.usage.modality_details` OTLP attribute
carry the same data. Deploy a compatible Sigil server before releasing SDK changes.
The server owns persisted pricing; SDK callers cannot provide a billed amount.

## Provider support

| Path | Support in this change |
| --- | --- |
| Gemini GenerateContent | Go, Python, JS, Java and .NET preserve supplied modality partitions; thinking joins output text once. Tool-use prompt tokens remain explicitly unpriceable. |
| OpenAI Responses usage | Go, Python, JS, Java and .NET preserve supplied text/image details. Cached modality counts are used only if the response supplies them. |
| OpenAI Images | Manual instrumentation using Python `from_openai_images`, JS `imageUsage`, Go `ApplyModalityJSON(..., "openai")`, or Java/.NET partition helpers. No automatic Images endpoint wrapper. |
| Gemini Interactions | Manual final-usage mapping with Python `from_gemini_interactions`, JS `interactionsUsage`, or Go `ApplyModalityJSON(..., "interactions")` after aggregate normalization. No automatic polling wrapper. |
| Responses image-generation tools | No automatic child-generation extraction; instrument the image call separately only when its actual model and usage are available. Do not claim parent token cost covers tool charges. |

For manual calls, record only the final cumulative usage, not both cumulative and
per-step usage. Use one stable generation ID for a completed provider operation
across retries/polls. Streaming fragments are not independently billable calls.
Never copy parent aggregate usage to an image-tool child or guess its model.

These are token-cost estimates, not invoice totals. Search, grounding, cache
storage, batch discounts and other non-token charges are outside this contract.
The new provider shape tests are synthetic examples of documented response fields,
not captured production responses or evidence of endpoint availability.

Go and .NET Responses mappers read the provider SDK's retained wire JSON so new
modality fields survive even when the generated typed usage class predates them.
