package nixflux

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

func marshalPythonJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	raw := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	return escapeJSONNonASCII(raw), nil
}

func escapeJSONNonASCII(raw []byte) []byte {
	var result bytes.Buffer
	inString := false
	escaped := false
	for len(raw) > 0 {
		r, size := utf8.DecodeRune(raw)
		chunk := raw[:size]
		raw = raw[size:]
		if !inString {
			result.Write(chunk)
			if r == '"' {
				inString = true
			}
			continue
		}
		if escaped {
			result.Write(chunk)
			escaped = false
			continue
		}
		if r == '\\' {
			result.Write(chunk)
			escaped = true
			continue
		}
		if r == '"' {
			result.Write(chunk)
			inString = false
			continue
		}
		if r <= 0x7f {
			result.Write(chunk)
			continue
		}
		if r <= 0xffff {
			fmt.Fprintf(&result, "\\u%04x", r)
		} else {
			pair := utf16.Encode([]rune{r})
			fmt.Fprintf(&result, "\\u%04x\\u%04x", pair[0], pair[1])
		}
	}
	return result.Bytes()
}
