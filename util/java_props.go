package util

import "strings"

var javaProps = make(map[string]string)

// SetJavaProps extracts Java-style -Dkey=value arguments from args, stores them,
// and returns a new slice with those arguments removed. The returned slice can be
// passed to flag.Parse or used directly. This lets the application support both
// standard Go flags and the existing -Denv.name=xxx convention without conflicts.
func SetJavaProps(args []string) []string {
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.HasPrefix(arg, "-D") {
			kv := strings.TrimPrefix(arg, "-D")
			key, value, _ := strings.Cut(kv, "=")
			javaProps[key] = value
			continue
		}
		filtered = append(filtered, arg)
	}
	return filtered
}

// JavaProp returns the value of a previously extracted -Dkey=value argument.
// If the argument was not provided, it returns the empty string.
func JavaProp(key string) string {
	return javaProps[key]
}
