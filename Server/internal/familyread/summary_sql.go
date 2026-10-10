package familyread

// microseconds converts a timestamptz expression to whole microseconds since
// the epoch. extract returns an exact numeric, so differences are exact.
func microseconds(expression string) string {
	return "(extract(epoch FROM " + expression + ") * 1000000)::bigint"
}
