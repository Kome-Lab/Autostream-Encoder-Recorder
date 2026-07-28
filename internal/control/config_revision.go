package control

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

const configRevisionEnv = "AUTOSTREAM_CONFIG_REVISION"

// ConfigRevisionFromEnv returns the locally applied service configuration
// revision. Revision 1 is the compatibility default for existing installs.
func ConfigRevisionFromEnv() (int64, error) {
	raw := os.Getenv(configRevisionEnv)
	if raw == "" {
		return 1, nil
	}
	if raw != strings.TrimSpace(raw) {
		return 0, errors.New(configRevisionEnv + " must be an unpadded positive integer")
	}
	if raw[0] == '0' {
		return 0, errors.New(configRevisionEnv + " must not contain leading zeroes")
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, errors.New(configRevisionEnv + " must contain decimal digits only")
		}
	}
	revision, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || revision < 1 {
		return 0, errors.New(configRevisionEnv + " must be an integer greater than or equal to 1")
	}
	return revision, nil
}
