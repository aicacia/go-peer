package simplepeer

import (
	"reflect"
)

func toJSON(data interface{}) interface{} {
	return valueToJSON(reflect.ValueOf(data))
}

func valueToJSON(value reflect.Value) interface{} {
	valueType := value.Type()
	switch valueType.Kind() {
	case reflect.Slice:
		fallthrough
	case reflect.Array:
		result := make([]interface{}, value.Len())
		for i := 0; i < value.Len(); i++ {
			result[i] = valueToJSON(value.Index(i))
		}
		return result
	case reflect.Struct:
		result := make(map[string]interface{}, value.NumField())
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			if !field.CanInterface() {
				continue
			}
			fieldType := valueType.Field(i)
			tag := fieldType.Tag.Get("json")
			if tag == "" {
				tag = fieldType.Name
			}
			if tag != "-" {
				result[tag] = valueToJSON(field)
			}
		}
		return result
	default:
		return value.Interface()
	}
}
