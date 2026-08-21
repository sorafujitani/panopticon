package panopticon

import (
	"fmt"

	"github.com/pelletier/go-toml/v2"
)

// ParseTOML decodes workflow and repository configuration with a standard
// TOML library so Python tomllib-compatible files keep loading.
func ParseTOML(content string) (map[string]any, error) {
	var root map[string]any
	if err := toml.Unmarshal([]byte(content), &root); err != nil {
		if decode, ok := err.(*toml.DecodeError); ok {
			row, _ := decode.Position()
			return nil, fmt.Errorf("line %d: %s", row, decode.Error())
		}
		return nil, err
	}
	if root == nil {
		root = map[string]any{}
	}
	normalized, ok := normalizeTOML(root).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("TOML root must be a table")
	}
	return normalized, nil
}

func normalizeTOML(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = normalizeTOML(item)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, normalizeTOML(item))
		}
		return result
	case []map[string]any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, normalizeTOML(item))
		}
		return result
	default:
		return value
	}
}
