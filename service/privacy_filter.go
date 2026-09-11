package service

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	privacyfilter "privacyfilter/filter"
)

const PrivacyFilterRedactedCountContextKey = "privacy_filter_redacted_count"

type PrivacyFilterStats struct {
	Hit   bool
	Count int
}

var privacyFilterCache struct {
	sync.Mutex
	path   string
	filter *privacyfilter.Filter
}

func IsPrivacyFilterEnabled() bool {
	return operation_setting.GetPrivacyFilterSetting().Enabled
}

func RedactPrivacyText(text string) (string, PrivacyFilterStats, error) {
	if !IsPrivacyFilterEnabled() || text == "" {
		return text, PrivacyFilterStats{}, nil
	}
	f, err := getPrivacyFilter()
	if err != nil {
		return "", PrivacyFilterStats{}, err
	}
	res := f.Redact(text)
	return res.Redacted, PrivacyFilterStats{
		Hit:   res.Hit,
		Count: res.Count,
	}, nil
}

func ApplyPrivacyFilterToJSON(c *gin.Context, data []byte) ([]byte, error) {
	redacted, stats, err := RedactPrivacyJSON(data)
	if err != nil {
		return nil, err
	}
	RecordPrivacyFilterStats(c, stats)
	return redacted, nil
}

func RedactPrivacyJSON(data []byte) ([]byte, PrivacyFilterStats, error) {
	if !IsPrivacyFilterEnabled() || len(data) == 0 {
		return data, PrivacyFilterStats{}, nil
	}
	if !gjson.ValidBytes(data) {
		return nil, PrivacyFilterStats{}, fmt.Errorf("invalid json")
	}

	redactor := privacyJSONRedactor{}
	if err := redactor.collect(gjson.ParseBytes(data), ""); err != nil {
		return nil, PrivacyFilterStats{}, err
	}
	if !redactor.stats.Hit {
		return data, redactor.stats, nil
	}

	size := len(data)
	for _, replacement := range redactor.replacements {
		size += len(replacement.Value) - int(replacement.End-replacement.Start)
	}
	redacted := make([]byte, 0, size)
	var offset int64
	for _, replacement := range redactor.replacements {
		redacted = append(redacted, data[offset:replacement.Start]...)
		redacted = append(redacted, replacement.Value...)
		offset = replacement.End
	}
	redacted = append(redacted, data[offset:]...)
	return redacted, redactor.stats, nil
}

// RedactPrivacyJSONStorage leaves ownership of the input with the caller and
// returns it unchanged when there are no hits. Disk input is traversed token by
// token; unselected strings are validated in bounded chunks. Only keys, selected
// strings, scalar literals and replacement text need to be materialized.
func RedactPrivacyJSONStorage(storage common.BodyStorage, maxBytes int64) (common.BodyStorage, PrivacyFilterStats, error) {
	if storage.Size() > maxBytes {
		return nil, PrivacyFilterStats{}, common.ErrRequestBodyTooLarge
	}
	if !IsPrivacyFilterEnabled() || storage.Size() == 0 {
		return storage, PrivacyFilterStats{}, nil
	}
	redactor := privacyJSONRedactor{}
	if !storage.IsDisk() {
		data, err := storage.Bytes()
		if err != nil {
			return nil, PrivacyFilterStats{}, err
		}
		if !gjson.ValidBytes(data) {
			return nil, PrivacyFilterStats{}, fmt.Errorf("invalid json")
		}
		if err := redactor.collect(gjson.ParseBytes(data), ""); err != nil {
			return nil, PrivacyFilterStats{}, err
		}
	} else {
		reader, err := storage.NewReader()
		if err != nil {
			return nil, PrivacyFilterStats{}, err
		}
		defer reader.Close()
		scanner := privacyJSONScanner{reader: bufio.NewReaderSize(reader, 32<<10)}
		token, err := scanner.next(false)
		if err != nil {
			return nil, PrivacyFilterStats{}, err
		}
		if err := redactor.collectStream(&scanner, token, ""); err != nil {
			return nil, PrivacyFilterStats{}, err
		}
		if _, err := scanner.next(false); !errors.Is(err, io.EOF) {
			return nil, PrivacyFilterStats{}, fmt.Errorf("invalid json: expected end of input")
		}
	}
	result, err := common.ReplaceBodyStorage(storage, redactor.replacements, maxBytes)
	if err != nil {
		return nil, PrivacyFilterStats{}, err
	}
	return result, redactor.stats, nil
}

func ApplyPrivacyFilterToFormValues(c *gin.Context, values map[string][]string) error {
	if !IsPrivacyFilterEnabled() || len(values) == 0 {
		return nil
	}
	var total PrivacyFilterStats
	for key, items := range values {
		if !shouldRedactPrivacyValue(key) {
			continue
		}
		for i := range items {
			redacted, stats, err := RedactPrivacyText(items[i])
			if err != nil {
				return err
			}
			items[i] = redacted
			total.Add(stats)
		}
		values[key] = items
	}
	RecordPrivacyFilterStats(c, total)
	return nil
}

func RecordPrivacyFilterStats(c *gin.Context, stats PrivacyFilterStats) {
	if c == nil || !stats.Hit {
		return
	}
	prevCount := c.GetInt(PrivacyFilterRedactedCountContextKey)
	c.Set(PrivacyFilterRedactedCountContextKey, prevCount+stats.Count)
}

func PrivacyFilterError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("privacy filter failed: %w", err)
}

func getPrivacyFilter() (*privacyfilter.Filter, error) {
	setting := operation_setting.GetPrivacyFilterSetting()
	path := strings.TrimSpace(setting.GitleaksTOML)

	privacyFilterCache.Lock()
	defer privacyFilterCache.Unlock()

	if privacyFilterCache.filter != nil && privacyFilterCache.path == path {
		return privacyFilterCache.filter, nil
	}

	f, err := privacyfilter.New(path)
	if err != nil {
		return nil, err
	}
	privacyFilterCache.path = path
	privacyFilterCache.filter = f
	return f, nil
}

func (s *PrivacyFilterStats) Add(other PrivacyFilterStats) {
	if other.Hit {
		s.Hit = true
	}
	s.Count += other.Count
}

type privacyJSONRedactor struct {
	filter       *privacyfilter.Filter
	stats        PrivacyFilterStats
	replacements []common.BodyReplacement
}

func (r *privacyJSONRedactor) collect(value gjson.Result, parentKey string) error {
	if value.IsObject() || value.IsArray() {
		object := value.IsObject()
		var walkErr error
		value.ForEach(func(key, child gjson.Result) bool {
			childKey := parentKey
			if object {
				childKey = key.String()
			}
			walkErr = r.collect(child, childKey)
			return walkErr == nil
		})
		return walkErr
	}
	if value.Type == gjson.String && shouldRedactPrivacyValue(parentKey) {
		return r.redact(value.String(), int64(value.Index), int64(value.Index+len(value.Raw)))
	}
	return nil
}

func (r *privacyJSONRedactor) redact(text string, start, end int64) error {
	if text == "" {
		return nil
	}
	if r.filter == nil {
		var err error
		r.filter, err = getPrivacyFilter()
		if err != nil {
			return err
		}
	}
	result := r.filter.Redact(text)
	if !result.Hit {
		return nil
	}
	encoded, err := common.Marshal(result.Redacted)
	if err != nil {
		return err
	}
	r.stats.Add(PrivacyFilterStats{Hit: true, Count: result.Count})
	r.replacements = append(r.replacements, common.BodyReplacement{Start: start, End: end, Value: encoded})
	return nil
}

func shouldRedactPrivacyValue(key string) bool {
	switch strings.ToLower(key) {
	case "content", "text", "input", "prompt", "prefix", "suffix",
		"instruction", "instructions", "query", "document", "documents",
		"system", "title", "tags", "gpt_description_prompt", "description",
		"negative_prompt", "ref_text":
		return true
	default:
		return false
	}
}
