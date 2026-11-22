package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// dayWeekUnits 匹配 time.ParseDuration 不支持的 day 与 week 后缀。
// 配置里写 30d / 6h 比写 720h 可读得多。
var dayWeekUnits = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)([dw])`)

// normalizeDuration 把 day 与 week 单位换算成小时，好让
// time.ParseDuration 能处理该值。
func normalizeDuration(s string) string {
	return dayWeekUnits.ReplaceAllStringFunc(s, func(match string) string {
		parts := dayWeekUnits.FindStringSubmatch(match)
		value, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return match
		}
		hours := value * 24
		if parts[2] == "w" {
			hours = value * 24 * 7
		}
		return strconv.FormatFloat(hours, 'f', -1, 64) + "h"
	})
}

// Duration 是以 Go duration 字符串（"30m"、"87600h"）序列化/反序列化的
// time.Duration。配置里不写裸纳秒，避免可读性问题。
type Duration time.Duration

// D 返回底层的 time.Duration。
func (d Duration) D() time.Duration { return time.Duration(d) }

// String 实现 fmt.Stringer。
func (d Duration) String() string { return time.Duration(d).String() }

// IsZero 报告该时长是否未设置。
func (d Duration) IsZero() bool { return time.Duration(d) == 0 }

// MarshalJSON 实现 json.Marshaler。
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// MarshalYAML 实现 yaml.Marshaler，保持与 JSON 相同的字符串形式。
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = 0
		return nil
	}
	// 兼容数字形式（纳秒），便于程序生成配置。
	var num int64
	if err := json.Unmarshal(b, &num); err == nil {
		*d = Duration(num)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("invalid duration %s: %w", string(b), err)
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(normalizeDuration(s))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Set 解析时长字符串；空字符串会把值重置为零。
func (d *Duration) Set(s string) error {
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(normalizeDuration(s))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}
