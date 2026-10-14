package gemini

// Schema is JSON Schema expressed with JSON-compatible Go values. The Gemini
// backend sends it through ResponseJsonSchema so object closure is preserved.
type Schema map[string]any

func koreanTranslationSchema() Schema {
	return Schema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"korean"},
		"properties": map[string]any{
			"korean": map[string]any{
				"type": "string",
			},
		},
	}
}

func englishTranslationSchema() Schema {
	return Schema{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"english"},
		"properties": map[string]any{
			"english": map[string]any{
				"type": "string",
			},
		},
	}
}
