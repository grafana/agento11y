package sigil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxRatingConversationIDLen = 255
	maxRatingIDLen             = 128
	maxRatingGenerationIDLen   = 255
	maxRatingActorIDLen        = 255
	maxRatingSourceLen         = 64
	maxRatingCommentBytes      = 4096
	maxRatingMetadataBytes     = 16 * 1024
)

type ConversationRatingValue string

const (
	ConversationRatingValueGood ConversationRatingValue = "CONVERSATION_RATING_VALUE_GOOD"
	ConversationRatingValueBad  ConversationRatingValue = "CONVERSATION_RATING_VALUE_BAD"
)

type ConversationRatingInput struct {
	RatingID     string                  `json:"rating_id"`
	Rating       ConversationRatingValue `json:"rating"`
	Comment      string                  `json:"comment,omitempty"`
	Metadata     map[string]any          `json:"metadata,omitempty"`
	GenerationID string                  `json:"generation_id,omitempty"`
	RaterID      string                  `json:"rater_id,omitempty"`
	Source       string                  `json:"source,omitempty"`
}

type ConversationRating struct {
	RatingID       string         `json:"rating_id"`
	ConversationID string         `json:"conversation_id"`
	GenerationID   string         `json:"generation_id,omitempty"`
	Rating         string         `json:"rating"`
	Comment        string         `json:"comment,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	RaterID        string         `json:"rater_id,omitempty"`
	Source         string         `json:"source,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
}

type ConversationRatingSummary struct {
	TotalCount    int       `json:"total_count"`
	GoodCount     int       `json:"good_count"`
	BadCount      int       `json:"bad_count"`
	LatestRating  string    `json:"latest_rating,omitempty"`
	LatestRatedAt time.Time `json:"latest_rated_at"`
	LatestBadAt   time.Time `json:"latest_bad_at"`
	HasBadRating  bool      `json:"has_bad_rating"`
}

type SubmitConversationRatingResponse struct {
	Rating  ConversationRating        `json:"rating"`
	Summary ConversationRatingSummary `json:"summary"`
}

func (c *Client) SubmitConversationRating(ctx context.Context, conversationID string, input ConversationRatingInput) (*SubmitConversationRatingResponse, error) {
	if c == nil {
		return nil, ErrNilClient
	}

	normalizedConversationID := strings.TrimSpace(conversationID)
	if normalizedConversationID == "" {
		return nil, fmt.Errorf("%w: conversation id is required", ErrRatingValidationFailed)
	}
	if len(normalizedConversationID) > maxRatingConversationIDLen {
		return nil, fmt.Errorf("%w: conversation id is too long", ErrRatingValidationFailed)
	}

	// Check content capture resolver — strip comment when MetadataOnly.
	resolverMode := callContentCaptureResolver(ctx, c.config.ContentCaptureResolver, input.Metadata)
	effectiveMode := resolveContentCaptureMode(resolverMode, resolveClientContentCaptureMode(c.config.ContentCapture))
	if effectiveMode == ContentCaptureModeMetadataOnly {
		input.Comment = ""
	}

	normalizedInput, err := normalizeConversationRatingInput(input)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRatingValidationFailed, err)
	}

	baseURL, err := baseURLFromAPIEndpoint(c.config.API.Endpoint, insecureValue(c.config.GenerationExport.Insecure))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRatingTransportFailed, err)
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/api/v1/conversations/" + url.PathEscape(normalizedConversationID) + "/ratings"

	payload, err := json.Marshal(normalizedInput)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal rating request: %v", ErrRatingTransportFailed, err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: build rating request: %v", ErrRatingTransportFailed, err)
	}
	request.Header.Set("Content-Type", "application/json")
	for key, value := range c.config.GenerationExport.Headers {
		request.Header.Set(key, value)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: submit rating request: %v", ErrRatingTransportFailed, err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read rating response: %v", ErrRatingTransportFailed, err)
	}

	bodyText := strings.TrimSpace(string(body))
	switch response.StatusCode {
	case http.StatusOK:
		var out SubmitConversationRatingResponse
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("%w: decode rating response: %v", ErrRatingTransportFailed, err)
		}
		return &out, nil
	case http.StatusBadRequest:
		return nil, fmt.Errorf("%w: %s", ErrRatingValidationFailed, ratingErrorText(bodyText, response.StatusCode))
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: %s", ErrRatingConflict, ratingErrorText(bodyText, response.StatusCode))
	default:
		return nil, fmt.Errorf("%w: status %d: %s", ErrRatingTransportFailed, response.StatusCode, ratingErrorText(bodyText, response.StatusCode))
	}
}

func normalizeConversationRatingInput(input ConversationRatingInput) (ConversationRatingInput, error) {
	normalized := ConversationRatingInput{
		RatingID:     strings.TrimSpace(input.RatingID),
		Rating:       ConversationRatingValue(strings.TrimSpace(string(input.Rating))),
		Comment:      strings.TrimSpace(input.Comment),
		Metadata:     input.Metadata,
		GenerationID: strings.TrimSpace(input.GenerationID),
		RaterID:      strings.TrimSpace(input.RaterID),
		Source:       strings.TrimSpace(input.Source),
	}

	if normalized.RatingID == "" {
		return ConversationRatingInput{}, fmt.Errorf("rating id is required")
	}
	if len(normalized.RatingID) > maxRatingIDLen {
		return ConversationRatingInput{}, fmt.Errorf("rating id is too long")
	}
	if normalized.Rating != ConversationRatingValueGood && normalized.Rating != ConversationRatingValueBad {
		return ConversationRatingInput{}, fmt.Errorf("rating must be CONVERSATION_RATING_VALUE_GOOD or CONVERSATION_RATING_VALUE_BAD")
	}
	if len(normalized.Comment) > maxRatingCommentBytes {
		return ConversationRatingInput{}, fmt.Errorf("comment is too long")
	}
	if len(normalized.GenerationID) > maxRatingGenerationIDLen {
		return ConversationRatingInput{}, fmt.Errorf("generation id is too long")
	}
	if len(normalized.RaterID) > maxRatingActorIDLen {
		return ConversationRatingInput{}, fmt.Errorf("rater id is too long")
	}
	if len(normalized.Source) > maxRatingSourceLen {
		return ConversationRatingInput{}, fmt.Errorf("source is too long")
	}
	if normalized.Metadata != nil {
		metadataBytes, err := json.Marshal(normalized.Metadata)
		if err != nil {
			return ConversationRatingInput{}, fmt.Errorf("metadata must be valid JSON")
		}
		if len(metadataBytes) > maxRatingMetadataBytes {
			return ConversationRatingInput{}, fmt.Errorf("metadata is too large")
		}
	}

	return normalized, nil
}

func baseURLFromAPIEndpoint(endpoint string, insecure bool) (string, error) {
	trimmed := strings.TrimSpace(endpoint)
	if trimmed == "" {
		return "", fmt.Errorf("api endpoint is required")
	}

	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("parse api endpoint URL: %w", err)
		}
		if strings.TrimSpace(parsed.Host) == "" {
			return "", fmt.Errorf("api endpoint host is required")
		}
		return parsed.Scheme + "://" + parsed.Host, nil
	}

	withoutScheme := strings.TrimPrefix(trimmed, "grpc://")
	host := withoutScheme
	if slash := strings.Index(host, "/"); slash >= 0 {
		host = host[:slash]
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("api endpoint host is required")
	}

	scheme := "https://"
	if insecure {
		scheme = "http://"
	}
	return scheme + host, nil
}

func ratingErrorText(body string, statusCode int) string {
	if body != "" {
		return body
	}
	return http.StatusText(statusCode)
}
