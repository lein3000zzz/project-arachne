package parser

import (
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
)

const habrShaped = `<!doctype html><html><head><title>Разбираемся с витамином D</title></head>
<body class="page comments-open">
<article>
  <h1>Разбираемся с витамином D — пить или нет?</h1>
  <div class="stats">
    <div class="tm-article-reading-time"><span class="tm-article-reading-time__label">10 мин</span></div>
    <span class="tm-icon-counter reach-counter"><span title="53395">53K</span></span>
  </div>
  <div class="share-buttons"><a href="/s/vk">ВКонтакте</a><a href="/s/tg">Telegram</a></div>
  <p>Инженер производства приносит анализ: уровень витамина D ниже нормы. Лицо у него как у
  человека, которому сообщили плохую новость, и он просит объяснить, что с этим делать.</p>
  <p>Рекомендаций аж три штуки, и они противоречат друг другу. Давайте разбираться, почему так
  получилось и какой из них стоит верить в первую очередь.</p>
  <p>Наши лучшие посты:</p>
  <ul>
    <li><p><a href="/a/1">Как мы ели лубрикант</a>;</p></li>
    <li><p><a href="/a/2">2022 год для производства</a>;</p></li>
    <li><p><a href="/a/3">Клонирование растений</a>;</p></li>
    <li><p><a href="/a/4">Кто живёт у вас на лице</a>.</p></li>
  </ul>
</article>
</body></html>`

func TestHabrWidgetsAndLinkListsAreRemoved(t *testing.T) {
	res := parse(t, newTestParser(t), habrShaped, "https://habr.com/ru/articles/1/", "text/html")

	for _, leaked := range []string{"10 мин", "53K", "ВКонтакте", "Наши лучшие посты", "лубрикант", "Клонирование"} {
		if strings.Contains(res.Content, leaked) {
			t.Errorf("%q leaked into content:\n%s", leaked, res.Content)
		}
	}

	for _, kept := range []string{"Инженер производства приносит анализ", "Давайте разбираться"} {
		if !strings.Contains(res.Content, kept) {
			t.Errorf("article text %q was removed:\n%s", kept, res.Content)
		}
	}
}

func cleanedText(t *testing.T, page string) string {
	t.Helper()

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}

	removeBoilerplate(doc.Selection)
	removeLinkLists(doc.Selection)

	return normalizeWhitespace(doc.Text())
}

func TestCleaningKeepsContentThatOnlyLooksLikeFurniture(t *testing.T) {
	cases := []struct {
		name string
		page string
		keep string
	}{
		{"token inside a longer word", `<div class="shareholder-report">Quarterly results for shareholders.</div>`, "Quarterly results"},
		{"commentary is not comments", `<section class="commentary">An editor's commentary.</section>`, "editor's commentary"},
		{"body with a furniture class", `<body class="post comments-open"><p>The whole article lives here.</p></body>`, "whole article"},
		{"syntax highlighting in code", `<pre><code>x := 1 <span class="comment">// start at one</span></code></pre>`, "start at one"},
		{"large block with a furniture class", `<div class="related">` + strings.Repeat("Substantial prose that is really part of the page. ", 12) + `</div>`, "Substantial prose"},
		{"list with the odd link", `<ul><li>Pass a timeout in milliseconds; values below ten are clamped.</li><li>Retries use exponential backoff, see <a href="/r">retries</a>.</li><li>Connections are pooled per host and reused.</li></ul>`, "exponential backoff"},
		{"link-heavy infobox table", `<table><tr><th><a href="/p">Paradigm</a></th><td><a href="/m">Multi-paradigm</a>: <a href="/c">concurrent</a>, <a href="/i">imperative</a></td></tr><tr><th><a href="/d">Designed by</a></th><td><a href="/rg">Robert Griesemer</a>, <a href="/rp">Rob Pike</a></td></tr></table>`, "Robert Griesemer"},
		{"data table without links", `<table><tr><td>Warsaw</td><td>1.8M</td></tr><tr><td>Kraków</td><td>0.8M</td></tr></table>`, "Kraków"},
		{"two links are not a link list", `<ul><li><a href="/a">First</a></li><li><a href="/b">Second</a></li></ul>`, "Second"},
		{"heading anchored with a furniture word", `<h3 id="sharing">Share by communicating</h3><p>Do not communicate by sharing memory.</p>`, "Share by communicating"},
		{"content section anchored by id", `<section id="related-work"><p>Prior systems used BM25 alone.</p></section>`, "Prior systems"},
		{"heading with a furniture class", `<h2 class="comments-title">Comments on the design</h2>`, "Comments on the design"},
		{"one-item list with a linked date", `<ul><li><span>1.27.1</span> (<a href="/d">1 сентября</a> <a href="/y">2026</a>)<sup><a href="#r">[5]</a></sup></li></ul>`, "1.27.1"},
		{"list where most items are prose", `<ul><li><a href="/a">Alpha</a></li><li><a href="/b">Beta</a></li><li><a href="/c">Gamma</a></li><li>Delta is described here at length with no link at all.</li><li>Epsilon also carries its own explanation.</li><li>Zeta the same.</li></ul>`, "Delta is described"},
		{"body list whose terms are linked", `<p>В Go намеренно не включены:</p><ul><li>реализация <a href="/i">наследования</a>;</li><li><a href="/a">алгебраические типы данных</a>;</li><li><a href="/o">перегрузка функций</a>;</li><li><a href="/p">переопределение операторов</a>;</li><li>ряд частных возможностей, характерных для других языков.</li></ul>`, "алгебраические типы данных"},
		{"colon line before a normal list", `<p>Supported formats:</p><ul><li>JSON with a schema</li><li>CSV with a header row</li><li>Parquet</li></ul>`, "Supported formats"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanedText(t, tc.page); !strings.Contains(got, tc.keep) {
				t.Errorf("%q was removed; left: %q", tc.keep, got)
			}
		})
	}
}

func TestCleaningRemovesFurniture(t *testing.T) {
	cases := []struct {
		name string
		page string
		gone string
	}{
		{"share bar", `<p>Body text.</p><div class="post-share">Share on X</div>`, "Share on X"},
		{"breadcrumbs", `<nav class="breadcrumbs">Home / Docs</nav><p>Body text.</p>`, "Home / Docs"},
		{"reading time compound", `<span class="article_reading_time">5 min read</span><p>Body text.</p>`, "5 min read"},
		{"hidden element", `<p>Body text.</p><div hidden>Hidden promo</div>`, "Hidden promo"},
		{"aria hidden", `<p>Body text.</p><span aria-hidden="true">decorative</span>`, "decorative"},
		{"see also list", `<ul><li><a href="/a">Alpha</a></li><li><a href="/b">Beta</a></li><li><a href="/c">Gamma</a></li></ul><p>Body text.</p>`, "Gamma"},
		{"intro line goes with its list", `<p>Read also:</p><ul><li><a href="/a">One post</a></li><li><a href="/b">Two posts</a></li><li><a href="/c">Three posts</a></li></ul><p>Body text.</p>`, "Read also"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanedText(t, tc.page)
			if strings.Contains(got, tc.gone) {
				t.Errorf("%q survived: %q", tc.gone, got)
			}

			if !strings.Contains(got, "Body text.") {
				t.Errorf("real content went with it: %q", got)
			}
		})
	}
}

func TestNamesBoilerplate(t *testing.T) {
	yes := []string{"share", "post-share", "social_links", "tm-icon-counter", "breadcrumbs", "article-reading-time", "READ_TIME", "related posts"}
	no := []string{"", "content", "article-body", "shareholder", "commentary", "popularity-chart", "counterpoint"}

	for _, names := range yes {
		if !namesBoilerplate(names) {
			t.Errorf("namesBoilerplate(%q) = false", names)
		}
	}

	for _, names := range no {
		if namesBoilerplate(names) {
			t.Errorf("namesBoilerplate(%q) = true", names)
		}
	}
}
