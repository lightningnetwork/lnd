package fn

import (
	"fmt"
	"maps"
	"slices"
)

// KeySet converts a map into a Set containing the keys of the map.
func KeySet[K comparable, V any](m map[K]V) Set[K] {
	return NewSet(slices.Collect(maps.Keys(m))...)
}

// NewSubMapIntersect returns a sub-map of `m` containing only the keys found in
// both `m` and the `keys` slice.
func NewSubMapIntersect[K comparable, V any](m map[K]V, keys []K) map[K]V {
	result := make(map[K]V)
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}

		result[k] = v
	}

	return result
}

// NewSubMap creates a sub-map from a given map using specified keys. It errors
// if any of the keys is not found in the map.
func NewSubMap[K comparable, V any](m map[K]V, keys []K) (map[K]V, error) {
	result := make(map[K]V, len(keys))
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			return nil, fmt.Errorf("NewSubMap: missing key %v", k)
		}

		result[k] = v
	}

	return result, nil
}
