package records

import (
	"errors"
	"time"
)

// Policy is transported from the kernel's settings owner to logd. Internal
// batching/durability bounds are constants, not independently configurable knobs.
type Policy struct {
	Enabled      bool          `json:"enabled"`
	Level        string        `json:"level"`
	SplitBy      string        `json:"split_by"`
	SplitPeriod  string        `json:"split_period"`
	MaxFileSize  int64         `json:"max_file_size"`
	MaxTotalSize int64         `json:"max_total_size"`
	MaxAge       time.Duration `json:"max_age"`
}

func (p Policy) Validate() error {
	if p.Level != "debug" && p.Level != "info" && p.Level != "warn" && p.Level != "error" {
		return errors.New("invalid logging level")
	}
	if p.SplitBy != "none" && p.SplitBy != "source" {
		return errors.New("invalid logging split source")
	}
	switch p.SplitPeriod {
	case "none", "minute", "hour", "day", "week", "month", "year":
	default:
		return errors.New("invalid logging split period")
	}
	if p.MaxFileSize < MaxRecord || p.MaxTotalSize < p.MaxFileSize {
		return errors.New("log file limit must be at least 16 KiB and total must cover one file")
	}
	if p.MaxAge <= 0 {
		return errors.New("log maximum age must be positive")
	}
	return nil
}

func (p Policy) Allows(level string) bool {
	if !p.Enabled {
		return false
	}
	cutoff := 1
	switch p.Level {
	case "debug":
		cutoff = 0
	case "warn":
		cutoff = 2
	case "error":
		cutoff = 3
	}
	return Severity(level) >= cutoff
}

func (p Policy) Stream(source string) string {
	if p.SplitBy == "source" {
		return source
	}
	return "all"
}

func PeriodBoundary(now time.Time, period string) time.Time {
	now = now.UTC()
	switch period {
	case "minute":
		return now.Truncate(time.Minute).Add(time.Minute)
	case "hour":
		return now.Truncate(time.Hour).Add(time.Hour)
	case "day":
		return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	case "week":
		days := (8 - int(now.Weekday())) % 7
		if days == 0 {
			days = 7
		}
		return time.Date(now.Year(), now.Month(), now.Day()+days, 0, 0, 0, 0, time.UTC)
	case "month":
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	case "year":
		return time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}
