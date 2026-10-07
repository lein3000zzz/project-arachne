package networker

type FetchResult struct {
	// FinalURL is where redirects ended, as the server spelled it.
	FinalURL string
	// IdentityURL is the address the requested URL is only another name for: where
	// the leading run of permanent, or scheme- or slash-only, redirects ended. The
	// first temporary redirect - to a login page, a mirror, a "latest" version -
	// says nothing about identity, so the run stops there. Without redirects it is
	// the requested URL.
	IdentityURL string
	Body        []byte
	Status      int
	ContentType string
}

type Networker interface {
	Stop()
	Fetch(url string) (*FetchResult, error)
}
