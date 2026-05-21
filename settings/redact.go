package settings

import (
	"encoding/json"
	"reflect"
)

const redactedPlaceholder = "***REDACTED***"

// Redact returns a deep clone of s with every field tagged `redact:"true"`
// replaced by a placeholder. The clone is safe to marshal to JSON for logging.
// A nil input returns nil with no error.
func Redact(s *Settings) (*Settings, error) {
	if s == nil {
		return nil, nil
	}

	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	var clone Settings
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}

	redactValue(reflect.ValueOf(&clone).Elem())

	return &clone, nil
}

func redactValue(v reflect.Value) {
	if !v.IsValid() {
		return
	}

	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}

			if f.Tag.Get("redact") == "true" {
				zeroSecret(v.Field(i))
				continue
			}

			redactValue(v.Field(i))
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			redactValue(v.Elem())
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			redactValue(v.Index(i))
		}
	}
}

func zeroSecret(v reflect.Value) {
	if !v.CanSet() {
		return
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString(redactedPlaceholder)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String {
			for i := 0; i < v.Len(); i++ {
				v.Index(i).SetString(redactedPlaceholder)
			}

			return
		}

		v.Set(reflect.MakeSlice(v.Type(), 0, 0))
	default:
		v.Set(reflect.Zero(v.Type()))
	}
}
