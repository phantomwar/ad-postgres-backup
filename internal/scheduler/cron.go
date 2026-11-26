package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule represents a parsed cron schedule or fixed interval.
type Schedule interface {
	Next(after time.Time) time.Time
}

type everySchedule struct {
	interval time.Duration
}

func (e everySchedule) Next(after time.Time) time.Time {
	return after.Add(e.interval)
}

type cronSchedule struct {
	seconds [60]bool
	minutes [60]bool
	hours   [24]bool
	dom     [32]bool // 1..31
	months  [13]bool // 1..12
	dow     [7]bool  // 0..6 (0=Sunday)
	domWild bool
	dowWild bool
}

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
	"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var dowNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3, "THU": 4, "FRI": 5, "SAT": 6,
}

// Parse parses @descriptors (@daily, @hourly, @weekly, @monthly, @yearly, @every <dur>),
// standard 5-field cron expressions, or 6-field go-cron expressions (with leading seconds).
func Parse(spec string) (Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty schedule")
	}
	if strings.HasPrefix(spec, "@every ") {
		raw := strings.TrimSpace(strings.TrimPrefix(spec, "@every "))
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid @every duration %q: %w", raw, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("@every duration must be > 0 (got %s)", d)
		}
		return everySchedule{interval: d}, nil
	}

	switch strings.ToLower(spec) {
	case "@yearly", "@annually":
		spec = "0 0 0 1 1 *"
	case "@monthly":
		spec = "0 0 0 1 * *"
	case "@weekly":
		spec = "0 0 0 * * 0"
	case "@daily", "@midnight":
		spec = "0 0 0 * * *"
	case "@hourly":
		spec = "0 0 * * * *"
	}

	fields := strings.Fields(spec)
	if len(fields) == 5 {
		fields = append([]string{"0"}, fields...)
	}
	if len(fields) != 6 {
		return nil, fmt.Errorf("expected 5 or 6 cron fields, got %d in %q", len(fields), spec)
	}

	var cs cronSchedule
	var err error
	if _, err = parseField(fields[0], 0, 59, nil, cs.seconds[:]); err != nil {
		return nil, fmt.Errorf("seconds field %q: %w", fields[0], err)
	}
	if _, err = parseField(fields[1], 0, 59, nil, cs.minutes[:]); err != nil {
		return nil, fmt.Errorf("minutes field %q: %w", fields[1], err)
	}
	if _, err = parseField(fields[2], 0, 23, nil, cs.hours[:]); err != nil {
		return nil, fmt.Errorf("hours field %q: %w", fields[2], err)
	}
	if cs.domWild, err = parseField(fields[3], 1, 31, nil, cs.dom[:]); err != nil {
		return nil, fmt.Errorf("day-of-month field %q: %w", fields[3], err)
	}
	if _, err = parseField(fields[4], 1, 12, monthNames, cs.months[:]); err != nil {
		return nil, fmt.Errorf("month field %q: %w", fields[4], err)
	}
	if cs.dowWild, err = parseDowField(fields[5], cs.dow[:]); err != nil {
		return nil, fmt.Errorf("day-of-week field %q: %w", fields[5], err)
	}

	return &cs, nil
}

// Next returns the next activation time strictly after `after`.
func (c *cronSchedule) Next(after time.Time) time.Time {
	t := after.Truncate(time.Second).Add(time.Second)
	limit := after.AddDate(5, 0, 0)

	for t.Before(limit) {
		if !c.months[int(t.Month())] {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.hours[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !c.minutes[t.Minute()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, t.Location())
			continue
		}
		if !c.seconds[t.Second()] {
			t = t.Add(time.Second)
			continue
		}
		return t
	}
	return after.Add(24 * time.Hour)
}

func (c *cronSchedule) dayMatches(t time.Time) bool {
	domMatch := c.dom[t.Day()]
	dowMatch := c.dow[int(t.Weekday())]
	if c.domWild && c.dowWild {
		return true
	}
	if c.domWild {
		return dowMatch
	}
	if c.dowWild {
		return domMatch
	}
	return domMatch || dowMatch
}

func parseField(expr string, minVal, maxVal int, names map[string]int, out []bool) (bool, error) {
	isWild := expr == "*"
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return false, fmt.Errorf("empty list element")
		}
		step := 1
		rangeExpr := part
		if slash := strings.Split(part, "/"); len(slash) == 2 {
			rangeExpr = slash[0]
			s, err := strconv.Atoi(slash[1])
			if err != nil || s <= 0 {
				return false, fmt.Errorf("invalid step %q", slash[1])
			}
			step = s
		} else if len(slash) > 2 {
			return false, fmt.Errorf("too many slashes in %q", part)
		}

		var start, end int
		if rangeExpr == "*" {
			start, end = minVal, maxVal
		} else if dash := strings.Split(rangeExpr, "-"); len(dash) == 2 {
			var err error
			if start, err = parseAtom(dash[0], minVal, maxVal, names); err != nil {
				return false, err
			}
			if end, err = parseAtom(dash[1], minVal, maxVal, names); err != nil {
				return false, err
			}
			if start > end {
				return false, fmt.Errorf("range start %d > end %d", start, end)
			}
		} else if len(dash) == 1 {
			var err error
			start, err = parseAtom(dash[0], minVal, maxVal, names)
			if err != nil {
				return false, err
			}
			if strings.Contains(part, "/") {
				end = maxVal
			} else {
				end = start
			}
		} else {
			return false, fmt.Errorf("invalid range %q", part)
		}

		for v := start; v <= end; v += step {
			out[v] = true
		}
	}
	return isWild, nil
}

func parseDowField(expr string, out []bool) (bool, error) {
	var raw [8]bool
	wild, err := parseField(expr, 0, 7, dowNames, raw[:])
	if err != nil {
		return false, err
	}
	for i := 0; i < 7; i++ {
		out[i] = raw[i]
	}
	if raw[7] {
		out[0] = true
	}
	return wild, nil
}

func parseAtom(raw string, minVal, maxVal int, names map[string]int) (int, error) {
	if names != nil {
		if v, ok := names[strings.ToUpper(raw)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid token %q", raw)
	}
	if v < minVal || v > maxVal {
		return 0, fmt.Errorf("value %d out of bounds [%d..%d]", v, minVal, maxVal)
	}
	return v, nil
}
