package fixes

import (
	"github.com/thockin/klog-to-logr/fixer"
)

// Must panics if err is not nil, and returns fix otherwise.
func Must(fix fixer.Fix, err error) fixer.Fix {
	if err != nil {
		panic(err.Error())
	}
	return fix
}

