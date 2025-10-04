package utils

import (
	"crypto/rand"
	"encoding/hex"
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

func CorrectURLScheme(URL string) string {
	startURL := URL
	if u, err := url.Parse(startURL); err != nil || u.Scheme == "" || u.Host == "" {
		if parsed, err2 := url.Parse("https://" + URL); err2 == nil {
			startURL = parsed.String()
		}
	}
	return startURL
}

func DrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func GenerateID() (string, error) {
	bytes := make([]byte, 20)
	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
