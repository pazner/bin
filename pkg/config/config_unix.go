//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func checkDirExistsAndWritable(dir string) error {
	if fi, err := os.Stat(dir); err != nil {
		return fmt.Errorf("Error setting download path [%w]", err)
	} else if !fi.IsDir() {
		return errors.New("Download path is not a directory")
	}
	// TODO make this work in non unix platforms
	err := unix.Access(dir, unix.W_OK)
	return err
}
