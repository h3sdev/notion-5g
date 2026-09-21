package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// readBody lee el cuerpo con un límite de tamaño para no dejar que un cliente
// mal comportado (o un bug en la app) tumbe el proceso con un POST gigante.
func readBody(r *http.Request, maxBytes int64) ([]byte, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBytes)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("cuerpo inválido o demasiado grande: %w", err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("cuerpo vacío")
	}
	return b, nil
}

// splitJSONItems acepta tanto un objeto {"...": ...} como un arreglo [{...}, {...}]
// y siempre devuelve una lista de objetos individuales en bruto.
func splitJSONItems(body []byte) ([]json.RawMessage, error) {
	trimmed := body
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\n' || trimmed[0] == '\t' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("cuerpo vacío")
	}
	if trimmed[0] == '[' {
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, err
		}
		return items, nil
	}
	var obj json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	return []json.RawMessage{obj}, nil
}

func parseIntSafe(s string) (int, error) { return strconv.Atoi(s) }
