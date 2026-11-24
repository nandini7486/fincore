package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

var (
	errEmptyBody   = errors.New("request body is empty")
	errInvalidBody = errors.New("request body is not a single valid JSON object for this endpoint")
)

// decodeJSONBody strictly decodes one JSON object into dst:
//   - the body is capped at maxBytes,
//   - unknown fields are rejected, so a client can never send a field such
//     as "balance" and have it silently ignored,
//   - trailing data after the object is rejected.
//
// An empty body returns errEmptyBody, which callers may accept or reject.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errEmptyBody
		}
		return errInvalidBody
	}
	if dec.More() {
		return errInvalidBody
	}
	return nil
}
