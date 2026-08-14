package otelgenai

import "testing"

func TestEncodingDiagnostics(t *testing.T) {
	t.Parallel()

	emptyModality := ""
	cases := []struct {
		name              string
		systemInstruction bool
		part              Part
		wantPayload       string
		wantError         string
	}{
		{
			name:              "generic system instruction keeps type and extensions without a warning",
			systemInstruction: true,
			part: Part{
				Type:       PartTypeToolCall,
				Extensions: map[string]any{"vendor.source": "policy"},
			},
			wantPayload: `[{"type":"tool_call","vendor.source":"policy"}]`,
		},
		{
			name:              "generic system instruction reports dropped fields once",
			systemInstruction: true,
			part: Part{
				Type:       PartTypeToolCall,
				Name:       "weather",
				Extensions: map[string]any{"vendor.source": "policy"},
			},
			wantPayload: `[{"type":"tool_call","vendor.source":"policy"}]`,
			wantError:   `otelgenai: message part of type "tool_call" keeps only its type and its extensions: the schema's generic part has no other field`,
		},
		{
			name:              "untyped system instruction reports that it was dropped",
			systemInstruction: true,
			part:              Part{},
			wantPayload:       `[]`,
			wantError:         "otelgenai: drop message part with no type",
		},
		{
			name:        "blob reports omitted modality",
			part:        Part{Type: PartTypeBlob},
			wantPayload: `{"type":"blob","content":""}`,
			wantError:   "otelgenai: blob part has no modality; omitting the schema-required key",
		},
		{
			name:        "blob reports empty modality",
			part:        Part{Type: PartTypeBlob, Modality: &emptyModality},
			wantPayload: `{"type":"blob","content":""}`,
			wantError:   "otelgenai: blob part has no modality; omitting the schema-required key",
		},
		{
			name:        "file reports omitted modality",
			part:        Part{Type: PartTypeFile},
			wantPayload: `{"type":"file","file_id":""}`,
			wantError:   "otelgenai: file part has no modality; omitting the schema-required key",
		},
		{
			name:        "URI reports omitted modality",
			part:        Part{Type: PartTypeURI},
			wantPayload: `{"type":"uri","uri":""}`,
			wantError:   "otelgenai: uri part has no modality; omitting the schema-required key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var payload string
			var err error
			if tc.systemInstruction {
				payload, err = encodeSystemInstructions([]Part{tc.part})
			} else {
				encoded, encodeErr := encodePart(tc.part)
				payload = string(encoded)
				err = encodeErr
			}

			if payload != tc.wantPayload {
				t.Errorf("payload = %s, want %s", payload, tc.wantPayload)
			}
			gotError := ""
			if err != nil {
				gotError = err.Error()
			}
			if gotError != tc.wantError {
				t.Errorf("error = %q, want %q", gotError, tc.wantError)
			}
		})
	}
}
