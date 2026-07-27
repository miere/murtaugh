package define

import "fmt"

// stringSlice coerces an args value (nil, []string, or []any from JSON
// decoding) into []string. Any non-string element is rejected.
func stringSlice(v any) ([]string, error) {
	switch xs := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return xs, nil
	case []any:
		out := make([]string, 0, len(xs))
		for i, e := range xs {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("element %d is %T, want string", i, e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected array of strings, got %T", v)
	}
}
