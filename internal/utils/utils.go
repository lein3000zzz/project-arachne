package utils

import (
	"net/url"
	"time"
)

func GetBaseURL(urlToParse string) (string, error) {
	u, err := url.Parse(urlToParse)
	if err != nil {
		return "", err
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func DrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
