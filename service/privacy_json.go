package service

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

type privacyJSONToken struct {
	kind       byte
	text       string
	start, end int64
}

// privacyJSONScanner retains raw offsets and skips unselected strings in bounded
// chunks. A general-purpose Decoder.Token would allocate the entire decoded
// string even for an unselected multi-megabyte base64 attachment.
type privacyJSONScanner struct {
	reader *bufio.Reader
	offset int64
}

func (s *privacyJSONScanner) next(captureString bool) (privacyJSONToken, error) {
	var token privacyJSONToken
	for {
		ch, err := s.reader.ReadByte()
		if err != nil {
			return token, err
		}
		s.offset++
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			continue
		}
		token.kind, token.start = ch, s.offset-1
		break
	}
	switch token.kind {
	case '{', '}', '[', ']', ':', ',':
		token.end = s.offset
		return token, nil
	case '"':
		var raw []byte
		hasEscape := false
		escaped, unicodeDigits := false, 0
		for {
			chunk, err := s.reader.ReadSlice('"')
			s.offset += int64(len(chunk))
			closed := false
			for _, ch := range chunk {
				switch {
				case unicodeDigits > 0:
					if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
						return token, fmt.Errorf("invalid json unicode escape")
					}
					unicodeDigits--
				case escaped:
					switch ch {
					case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					case 'u':
						unicodeDigits = 4
					default:
						return token, fmt.Errorf("invalid json string escape")
					}
					escaped = false
				case ch == '\\':
					escaped = true
					hasEscape = true
				case ch == '"':
					closed = true
				case ch < 0x20:
					return token, fmt.Errorf("invalid json string control character")
				}
			}
			if captureString {
				if closed && len(raw) == 0 && !hasEscape {
					token.text, token.end = string(chunk[:len(chunk)-1]), s.offset
					return token, nil
				}
				if len(raw) == 0 {
					raw = append(raw, '"')
				}
				raw = append(raw, chunk...)
			}
			if closed {
				token.end = s.offset
				if captureString {
					var decoded string
					if err := common.Unmarshal(raw, &decoded); err != nil {
						return token, err
					}
					token.text = decoded
				}
				return token, nil
			}
			if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				return token, err
			}
		}
	default:
		// Validate numbers and literals without converting to float64, so huge
		// integers, exponents and negative zero keep their original spelling.
		raw := []byte{token.kind}
		for {
			ch, err := s.reader.ReadByte()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return token, err
			}
			if ch == ',' || ch == ']' || ch == '}' || ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
				if err := s.reader.UnreadByte(); err != nil {
					return token, err
				}
				break
			}
			s.offset++
			raw = append(raw, ch)
		}
		if !gjson.ValidBytes(raw) {
			return token, fmt.Errorf("invalid json literal")
		}
		token.kind, token.end = 'v', s.offset
		return token, nil
	}
}

// collectStream checks object/array grammar separately from lexical validation
// and records each selected string's original byte range, including duplicates.
func (r *privacyJSONRedactor) collectStream(s *privacyJSONScanner, token privacyJSONToken, parentKey string) error {
	switch token.kind {
	case '"':
		if shouldRedactPrivacyValue(parentKey) {
			return r.redact(token.text, token.start, token.end)
		}
		return nil
	case 'v':
		return nil
	case '{', '[':
		object := token.kind == '{'
		closing := byte(']')
		if object {
			closing = '}'
		}
		capture := object || shouldRedactPrivacyValue(parentKey)
		child, err := s.next(capture)
		if err != nil {
			return err
		}
		if child.kind == closing {
			return nil
		}
		for {
			childKey := parentKey
			if object {
				if child.kind != '"' {
					return fmt.Errorf("invalid json object key")
				}
				childKey = child.text
				separator, err := s.next(false)
				if err != nil || separator.kind != ':' {
					return fmt.Errorf("invalid json: expected colon")
				}
				child, err = s.next(shouldRedactPrivacyValue(childKey))
				if err != nil {
					return err
				}
			}
			if err := r.collectStream(s, child, childKey); err != nil {
				return err
			}
			separator, err := s.next(false)
			if err != nil {
				return err
			}
			if separator.kind == closing {
				return nil
			}
			if separator.kind != ',' {
				return fmt.Errorf("invalid json: expected comma")
			}
			child, err = s.next(capture)
			if err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("invalid json: expected value")
	}
}
