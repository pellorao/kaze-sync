//go:build !linux && !windows

package resources

import "errors"

func ReadSelf() (Reading, error) {
	return Reading{}, errors.New("process sampling unsupported on this OS")
}
