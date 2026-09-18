package appconfig

import "errors"

var (
	ErrInvalidConfig = errors.New("invalid config")
	ErrNotFound      = errors.New("config file not found")
)
