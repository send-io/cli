package tmpl

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type helper struct {
	fn       func(args []any, hash map[string]any) (any, error)
	min, max int // max -1: no limit
}

func (h helper) arity() string {
	switch {
	case h.min == h.max:
		return fmt.Sprintf("needs exactly %d argument(s)", h.min)
	case h.max < 0:
		return fmt.Sprintf("needs at least %d argument(s)", h.min)
	}
	return fmt.Sprintf("needs %d to %d arguments", h.min, h.max)
}

// blockHelpers are the built-in block helpers and their argument counts.
var blockHelpers = map[string]int{"if": 1, "unless": 1, "each": 1, "with": 1}

// HelperNames lists the value helpers (for documentation and errors).
func HelperNames() []string {
	out := make([]string, 0, len(helpers))
	for k := range helpers {
		out = append(out, k)
	}
	return out
}

var helpers map[string]helper

func init() {
	cmp := func(test func(c int) bool) helper {
		return helper{min: 2, max: 2, fn: func(a []any, _ map[string]any) (any, error) {
			c, ok := compare(a[0], a[1])
			return ok && test(c), nil
		}}
	}
	eq := helper{min: 2, max: 2, fn: func(a []any, _ map[string]any) (any, error) { return looseEqual(a[0], a[1]), nil }}
	ne := helper{min: 2, max: 2, fn: func(a []any, _ map[string]any) (any, error) { return !looseEqual(a[0], a[1]), nil }}
	lt := cmp(func(c int) bool { return c < 0 })
	gt := cmp(func(c int) bool { return c > 0 })
	or := helper{min: 1, max: -1, fn: func(a []any, _ map[string]any) (any, error) {
		for _, v := range a {
			if Truthy(v, false) {
				return v, nil
			}
		}
		return a[len(a)-1], nil
	}}
	helpers = map[string]helper{
		"eq": eq, "equals": eq,
		"ne": ne, "notEquals": ne,
		"lt": lt, "lessThan": lt,
		"gt": gt, "greaterThan": gt,
		"lte": cmp(func(c int) bool { return c <= 0 }),
		"gte": cmp(func(c int) bool { return c >= 0 }),
		"and": {min: 1, max: -1, fn: func(a []any, _ map[string]any) (any, error) {
			for _, v := range a {
				if !Truthy(v, false) {
					return false, nil
				}
			}
			return true, nil
		}},
		"or":  or,
		"not": {min: 1, max: 1, fn: func(a []any, _ map[string]any) (any, error) { return !Truthy(a[0], false), nil }},
		"length": {min: 1, max: 1, fn: func(a []any, _ map[string]any) (any, error) {
			switch x := a[0].(type) {
			case nil:
				return float64(0), nil
			case string:
				return float64(utf8.RuneCountInString(x)), nil
			}
			rv := reflect.ValueOf(a[0])
			switch rv.Kind() {
			case reflect.Slice, reflect.Array, reflect.Map:
				return float64(rv.Len()), nil
			}
			return float64(0), nil
		}},
		"lookup": {min: 2, max: 2, fn: func(a []any, _ map[string]any) (any, error) {
			v, _ := field(a[0], toString(a[1]))
			return v, nil
		}},
		"default": {min: 2, max: -1, fn: func(a []any, _ map[string]any) (any, error) {
			for _, v := range a {
				if v != nil && toString(v) != "" {
					return v, nil
				}
			}
			return a[len(a)-1], nil
		}},
		"uppercase": {min: 1, max: 1, fn: func(a []any, _ map[string]any) (any, error) { return strings.ToUpper(toString(a[0])), nil }},
		"lowercase": {min: 1, max: 1, fn: func(a []any, _ map[string]any) (any, error) { return strings.ToLower(toString(a[0])), nil }},
		// SendGrid: {{insert name "default=Customer"}}.
		"insert": {min: 1, max: 2, fn: func(a []any, hash map[string]any) (any, error) {
			if a[0] != nil && toString(a[0]) != "" {
				return a[0], nil
			}
			if len(a) == 2 {
				s := toString(a[1])
				if !strings.HasPrefix(s, "default=") {
					return nil, fmt.Errorf(`the second argument must be "default=..."`)
				}
				return strings.TrimPrefix(s, "default="), nil
			}
			if d, ok := hash["default"]; ok {
				return d, nil
			}
			return "", nil
		}},
		"formatDate": {min: 2, max: 3, fn: func(a []any, _ map[string]any) (any, error) {
			if a[0] == nil || toString(a[0]) == "" {
				return "", nil
			}
			t, ok := parseDate(a[0])
			if !ok {
				return nil, fmt.Errorf("cannot read %q as a date (use ISO 8601 such as 2026-10-07T14:30:00Z, or a Unix timestamp)", toString(a[0]))
			}
			if len(a) == 3 && a[2] != nil && toString(a[2]) != "" {
				loc, err := parseZone(toString(a[2]))
				if err != nil {
					return nil, err
				}
				t = t.In(loc)
			} else {
				t = t.UTC()
			}
			return FormatDate(t, toString(a[1])), nil
		}},
	}
}

// looseEqual: numbers compare by value (a number equals its numeric string,
// as templates often carry numbers as strings); otherwise same type and value.
func looseEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if isNumberType(a) || isNumberType(b) {
		x, ok1 := toNumber(a)
		y, ok2 := toNumber(b)
		if _, s := a.(string); s && !isNumberType(b) {
			return false
		}
		if _, s := b.(string); s && !isNumberType(a) {
			return false
		}
		return ok1 && ok2 && x == y
	}
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return reflect.DeepEqual(a, b)
}

// compare orders two values: numerically when both are numbers (or numeric
// strings), else as strings. ok=false when either is missing.
func compare(a, b any) (int, bool) {
	if a == nil || b == nil {
		return 0, false
	}
	x, ok1 := toNumber(a)
	y, ok2 := toNumber(b)
	if ok1 && ok2 {
		switch {
		case x < y:
			return -1, true
		case x > y:
			return 1, true
		}
		return 0, true
	}
	return strings.Compare(toString(a), toString(b)), true
}

var dateLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02", time.RFC1123Z, time.RFC1123}

func parseDate(v any) (time.Time, bool) {
	if t, ok := v.(time.Time); ok {
		return t, true
	}
	if n, ok := toNumber(v); ok {
		if n > 1e11 || n < -1e11 {
			return time.UnixMilli(int64(n)), true
		}
		sec := int64(n)
		return time.Unix(sec, int64((n-float64(sec))*1e9)), true
	}
	s := strings.TrimSpace(toString(v))
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

var zoneRe = regexp.MustCompile(`^([+-])(\d{2}):?(\d{2})$`)

func parseZone(s string) (*time.Location, error) {
	if s == "Z" || strings.EqualFold(s, "UTC") || strings.EqualFold(s, "GMT") {
		return time.UTC, nil
	}
	if m := zoneRe.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[2])
		mi, _ := strconv.Atoi(m[3])
		off := h*3600 + mi*60
		if m[1] == "-" {
			off = -off
		}
		return time.FixedZone(s, off), nil
	}
	if loc, err := time.LoadLocation(s); err == nil {
		return loc, nil
	}
	return nil, fmt.Errorf("unknown time zone %q (use an offset such as -0800 or +05:30)", s)
}

var dateTokRe = regexp.MustCompile(`\[[^\]]*\]|YYYY|YY|MMMM|MMM|MM|M|Do|DD|D|dddd|ddd|HH|H|hh|h|mm|m|ss|s|SSS|A|a|ZZ|Z|X|x`)

// FormatDate formats t with moment.js-style tokens, as SendGrid's formatDate
// does: YYYY YY MMMM MMM MM M Do DD D dddd ddd HH H hh h mm m ss s SSS A a Z
// ZZ X (Unix seconds) x (Unix ms); text in [brackets] is literal.
func FormatDate(t time.Time, layout string) string {
	return dateTokRe.ReplaceAllStringFunc(layout, func(tok string) string {
		h12 := t.Hour() % 12
		if h12 == 0 {
			h12 = 12
		}
		switch tok {
		case "YYYY":
			return fmt.Sprintf("%04d", t.Year())
		case "YY":
			return fmt.Sprintf("%02d", t.Year()%100)
		case "MMMM":
			return t.Month().String()
		case "MMM":
			return t.Month().String()[:3]
		case "MM":
			return fmt.Sprintf("%02d", int(t.Month()))
		case "M":
			return strconv.Itoa(int(t.Month()))
		case "Do":
			return ordinal(t.Day())
		case "DD":
			return fmt.Sprintf("%02d", t.Day())
		case "D":
			return strconv.Itoa(t.Day())
		case "dddd":
			return t.Weekday().String()
		case "ddd":
			return t.Weekday().String()[:3]
		case "HH":
			return fmt.Sprintf("%02d", t.Hour())
		case "H":
			return strconv.Itoa(t.Hour())
		case "hh":
			return fmt.Sprintf("%02d", h12)
		case "h":
			return strconv.Itoa(h12)
		case "mm":
			return fmt.Sprintf("%02d", t.Minute())
		case "m":
			return strconv.Itoa(t.Minute())
		case "ss":
			return fmt.Sprintf("%02d", t.Second())
		case "s":
			return strconv.Itoa(t.Second())
		case "SSS":
			return fmt.Sprintf("%03d", t.Nanosecond()/1e6)
		case "A":
			if t.Hour() < 12 {
				return "AM"
			}
			return "PM"
		case "a":
			if t.Hour() < 12 {
				return "am"
			}
			return "pm"
		case "Z":
			return t.Format("-07:00")
		case "ZZ":
			return t.Format("-0700")
		case "X":
			return strconv.FormatInt(t.Unix(), 10)
		case "x":
			return strconv.FormatInt(t.UnixMilli(), 10)
		}
		return tok[1 : len(tok)-1] // [literal]
	})
}

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}
