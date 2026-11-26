//go:build !linux && !freebsd

package clock

func kernelTimeState() (int, error) { return 0, ErrUnsupportedPlatform }

// New returns ErrUnsupportedPlatform: this operating system has no clock
// backend. The daemon's query and check modes, and every test, still work.
func New() (Clock, error) {
	return nil, ErrUnsupportedPlatform
}
