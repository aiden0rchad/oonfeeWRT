package model

import "fmt"

func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
