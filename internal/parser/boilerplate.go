package parser

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

// Class tokens that name page furniture rather than content. Matched as whole
// tokens, so "shareholder" or "commentary" are left alone. Ids are ignored: they
// are often anchors generated from the content itself, like Effective Go's
// <h3 id="sharing">Share by communicating</h3>.
var boilerplateTokens = map[string]struct{}{
	"share": {}, "sharing": {}, "social": {}, "breadcrumb": {}, "breadcrumbs": {},
	"subscribe": {}, "subscription": {}, "newsletter": {}, "promo": {}, "advert": {},
	"advertisement": {}, "ads": {}, "sponsor": {}, "sponsored": {}, "related": {},
	"recommended": {}, "recommendations": {}, "popular": {}, "comments": {}, "comment": {},
	"rating": {}, "votes": {}, "voting": {}, "counter": {}, "counters": {}, "cookie": {},
	"cookies": {}, "consent": {}, "toolbar": {}, "pagination": {}, "pager": {},
}

var boilerplateCompounds = []string{"reading-time", "readingtime", "read-time", "readtime", "view-count", "views-count"}

const (
	// Furniture is small. A larger match is more likely a container that happens to
	// carry such a class - WordPress puts "comments-open" on <body>.
	maxBoilerplateRunes = 400

	minLinkItems = 3
	minLinkShare = 0.6
	// A list is a link list only when nearly every item is one. Leaking a link list
	// costs a little noise; deleting a body list whose terms happen to be linked
	// makes that content unfindable.
	minLinkItemShare = 0.8
	maxIntroRunes    = 80
)

func removeBoilerplate(sel *goquery.Selection) {
	sel.Find(`[hidden], [aria-hidden="true"]`).Remove()
	removeChrome(sel)

	sel.Find("[class]").Each(func(_ int, s *goquery.Selection) {
		switch goquery.NodeName(s) {
		case "html", "body", "main", "article", "h1", "h2", "h3", "h4", "h5", "h6":
			return
		}

		if s.Closest("pre, code").Length() > 0 {
			return
		}

		class, _ := s.Attr("class")

		if namesBoilerplate(class) && textRunes(s) <= maxBoilerplateRunes {
			s.Remove()
		}
	})
}

// removeChrome drops the site around the content: navigation anywhere, and a
// header or footer that is the page's rather than an article's. Readability
// usually discards these itself, but on a page with little content it retries
// without that rule and returns the menu as the article - and a mega-menu whose
// items carry descriptions is no link list to removeLinkLists.
func removeChrome(sel *goquery.Selection) {
	sel.Find(`nav, [role="navigation"]`).Remove()

	sel.Find(`header, footer, [role="banner"], [role="contentinfo"]`).Each(func(_ int, s *goquery.Selection) {
		if s.Closest("main, article, [role=\"main\"]").Length() == 0 {
			s.Remove()
		}
	})
}

func removeLinkLists(sel *goquery.Selection) {
	sel.Find("ul, ol, dl").Each(func(_ int, list *goquery.Selection) {
		items := list.ChildrenFiltered("li, dt, dd")

		linkItems := 0
		items.Each(func(_ int, item *goquery.Selection) {
			if item.Find("a[href]").Length() > 0 && linkShare(item) >= minLinkShare {
				linkItems++
			}
		})

		if linkItems >= minLinkItems && float64(linkItems) >= minLinkItemShare*float64(items.Length()) {
			removeWithIntro(list)
		}
	})
}

func removeWithIntro(s *goquery.Selection) {
	if intro := s.Prev(); isListIntro(intro) {
		intro.Remove()
	}

	s.Remove()
}

func linkShare(s *goquery.Selection) float64 {
	total := textRunes(s)
	if total == 0 {
		return 0
	}

	return float64(textRunes(s.Find("a[href]"))) / float64(total)
}

func namesBoilerplate(names string) bool {
	names = strings.ReplaceAll(strings.ToLower(names), "_", "-")

	for _, compound := range boilerplateCompounds {
		if strings.Contains(names, compound) {
			return true
		}
	}

	for _, token := range strings.FieldsFunc(names, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if _, ok := boilerplateTokens[token]; ok {
			return true
		}
	}

	return false
}

func isListIntro(s *goquery.Selection) bool {
	if s.Length() == 0 {
		return false
	}

	switch goquery.NodeName(s) {
	case "p", "div", "span", "strong", "b", "h1", "h2", "h3", "h4", "h5", "h6":
	default:
		return false
	}

	text := normalizeWhitespace(s.Text())

	return text != "" && utf8.RuneCountInString(text) <= maxIntroRunes &&
		(strings.HasSuffix(text, ":") || strings.HasSuffix(text, "："))
}

func textRunes(s *goquery.Selection) int {
	return utf8.RuneCountInString(normalizeWhitespace(s.Text()))
}
