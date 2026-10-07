//go:build integration

package pages

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
	"web-crawler/internal/domain/data"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"go.uber.org/zap"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

// testRepo points a repo at a throwaway database, so the crawler's graph is never
// touched.
func testRepo(t *testing.T) *PageNeo4jRepo {
	t.Helper()

	driver, err := neo4j.NewDriverWithContext(
		envOr("NEO4J_URI", "bolt://localhost:7687"),
		neo4j.BasicAuth(envOr("NEO4J_USER", "neo4j"), envOr("NEO4J_PASSWORD", "testtest"), ""),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	name := fmt.Sprintf("pagestest%d", time.Now().UnixNano())

	system := func(query string) {
		if _, err := neo4j.ExecuteQuery(ctx, driver, query, nil, neo4j.EagerResultTransformer,
			neo4j.ExecuteQueryWithDatabase("system")); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	system("CREATE DATABASE " + name + " WAIT")

	t.Cleanup(func() {
		system("DROP DATABASE " + name + " IF EXISTS WAIT")
		_ = driver.Close(ctx)
	})

	repo := NewNeo4jRepo(zap.NewNop().Sugar(), driver)
	repo.database = name

	return repo
}

func (repo *PageNeo4jRepo) query(t *testing.T, query string, params map[string]any) []*neo4j.Record {
	t.Helper()

	res, err := neo4j.ExecuteQuery(context.Background(), repo.driver, query, params, neo4j.EagerResultTransformer,
		neo4j.ExecuteQueryWithDatabase(repo.database))
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}

	return res.Records
}

func (repo *PageNeo4jRepo) strings(t *testing.T, query string, params map[string]any) []string {
	t.Helper()

	var out []string

	for _, record := range repo.query(t, query, params) {
		v, _ := record.Get("v")
		out = append(out, v.(string))
	}

	sort.Strings(out)

	return out
}

func assertStrings(t *testing.T, what string, got []string, want ...string) {
	t.Helper()

	sort.Strings(want)

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestEnsureSchemaMergesAnOldGraph(t *testing.T) {
	repo := testRepo(t)

	repo.query(t, `
		CREATE (root:Page {url: "https://example.com", status: 200, title: "Home", lastUpdatedAt: "2026-09-01T00:00:00Z"})
		CREATE (:Page {url: "https://example.com/"})
		CREATE (a:Page {url: "https://example.com/a", status: 200})
		CREATE (b1:Page {url: "https://example.com/b"})
		CREATE (b2:Page {url: "https://example.com/b"})
		CREATE (c:Page {url: "HTTPS://Example.com/c#top"})
		CREATE (httpDoc:Page {url: "http://example.com/doc"})
		CREATE (:Page {url: "https://example.com/doc/", status: 200})
		CREATE (root)-[:LINKS_TO]->(a), (a)-[:LINKS_TO]->(b1), (a)-[:LINKS_TO]->(b2),
			(a)-[:LINKS_TO]->(c), (a)-[:LINKS_TO]->(httpDoc), (c)-[:LINKS_TO]->(root)
	`, nil)

	resolve := func(url string) (string, bool) {
		if url == "http://example.com/doc" {
			return "https://example.com/doc/", true
		}

		return "", false
	}

	removed, err := repo.EnsureSchema(context.Background(), resolve)
	if err != nil {
		t.Fatal(err)
	}

	assertStrings(t, "removed", sortedCopy(removed), "https://example.com", "HTTPS://Example.com/c#top", "http://example.com/doc")

	assertStrings(t, "pages", repo.strings(t, `MATCH (p:Page) RETURN p.url AS v`, nil),
		"https://example.com/", "https://example.com/a", "https://example.com/b", "https://example.com/c", "https://example.com/doc/")

	assertStrings(t, "links of /a", repo.strings(t, `MATCH (:Page {url: "https://example.com/a"})-[:LINKS_TO]->(p) RETURN p.url AS v`, nil),
		"https://example.com/b", "https://example.com/c", "https://example.com/doc/")

	assertStrings(t, "links into /", repo.strings(t, `MATCH (p)-[:LINKS_TO]->(:Page {url: "https://example.com/"}) RETURN p.url AS v`, nil),
		"https://example.com/c")

	assertStrings(t, "title of /", repo.strings(t, `MATCH (p:Page {url: "https://example.com/"}) RETURN p.title AS v`, nil), "Home")

	assertStrings(t, "aliases", repo.strings(t, `MATCH (a:Alias) RETURN a.url + " -> " + a.target AS v`, nil),
		"http://example.com/doc -> https://example.com/doc/")

	again, err := repo.EnsureSchema(context.Background(), resolve)
	if err != nil || len(again) != 0 {
		t.Errorf("second EnsureSchema = %v, %v; the migration must run once", again, err)
	}

	if _, err := neo4j.ExecuteQuery(context.Background(), repo.driver, `CREATE (:Page {url: "https://example.com/a"})`, nil,
		neo4j.EagerResultTransformer, neo4j.ExecuteQueryWithDatabase(repo.database)); err == nil {
		t.Error("a duplicate page must be rejected by the constraint")
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)

	return out
}

func page(url string, links []string, aliases ...string) *data.PageData {
	now := time.Now()

	return &data.PageData{URL: url, Status: 200, Links: links, Aliases: aliases, LastRunID: "run", LastUpdatedAt: now, FoundAt: now}
}

func TestSavePageFoldsAliases(t *testing.T) {
	repo := testRepo(t)

	if _, err := repo.EnsureSchema(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	save := func(p *data.PageData) {
		t.Helper()

		if err := repo.SavePage(p); err != nil {
			t.Fatal(err)
		}
	}

	save(page("https://example.com/", []string{"http://example.com/doc", "https://example.com/other"}))
	save(page("https://example.com/doc/", nil, "http://example.com/doc"))

	assertStrings(t, "links of / after the alias was found", repo.strings(t,
		`MATCH (:Page {url: "https://example.com/"})-[:LINKS_TO]->(p) RETURN p.url AS v`, nil),
		"https://example.com/doc/", "https://example.com/other")

	save(page("https://example.com/other", []string{"http://example.com/doc"}))

	assertStrings(t, "a link to a known alias", repo.strings(t,
		`MATCH (:Page {url: "https://example.com/other"})-[:LINKS_TO]->(p) RETURN p.url AS v`, nil),
		"https://example.com/doc/")

	save(page("https://example.com/docs/", nil, "https://example.com/doc/"))

	assertStrings(t, "chained aliases", repo.strings(t, `MATCH (a:Alias) RETURN a.url + " -> " + a.target AS v`, nil),
		"http://example.com/doc -> https://example.com/docs/", "https://example.com/doc/ -> https://example.com/docs/")

	save(page("https://example.com/doc/", nil))

	assertStrings(t, "aliases after the old URL became a page again", repo.strings(t,
		`MATCH (a:Alias) RETURN a.url AS v`, nil), "http://example.com/doc")

	assertStrings(t, "pages", repo.strings(t, `MATCH (p:Page) RETURN p.url AS v`, nil),
		"https://example.com/", "https://example.com/doc/", "https://example.com/docs/", "https://example.com/other")
}

func TestConcurrentSavesCreateOneNodePerURL(t *testing.T) {
	repo := testRepo(t)

	if _, err := repo.EnsureSchema(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for i := 0; i < 16; i++ {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			if err := repo.SavePage(page(fmt.Sprintf("https://example.com/p%d", i), []string{"https://example.com/shared"})); err != nil {
				t.Error(err)
			}
		}(i)
	}

	wg.Wait()

	assertStrings(t, "shared target", repo.strings(t, `MATCH (p:Page {url: "https://example.com/shared"}) RETURN p.url AS v`, nil),
		"https://example.com/shared")
}

func TestEnsureSchemaRecanonicalizesAfterANormalizerChange(t *testing.T) {
	repo := testRepo(t)

	if _, err := repo.EnsureSchema(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	repo.query(t, `
		CREATE (a:Page {url: "https://example.com/a", status: 200})
		CREATE (q1:Page {url: "https://EXAMPLE.com/q?x=1&y=2#top"})
		CREATE (q2:Page {url: "https://example.com/q?x=1&y=2"})
		CREATE (junk:Page {url: "https://&/"})
		CREATE (:Page {url: "https://proxy.golang.org,direct&quot", status: 200})
		CREATE (:Alias {url: "https://Example.com/r", target: "https://example.com/a"})
		CREATE (:Alias {url: "http://example.com/a", target: "https://example.com/a"})
		CREATE (a)-[:LINKS_TO]->(q1), (a)-[:LINKS_TO]->(junk)
		WITH 1 AS _
		MATCH (s:Schema {name: "urlnorm"}) SET s.version = 0
	`, nil)

	removed, err := repo.EnsureSchema(context.Background(), func(string) (string, bool) {
		t.Error("the server is asked only when the constraints are first created")
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}

	assertStrings(t, "removed", sortedCopy(removed), "https://EXAMPLE.com/q?x=1&y=2#top")

	assertStrings(t, "pages", repo.strings(t, `MATCH (p:Page) RETURN p.url AS v`, nil),
		"https://example.com/a", "https://example.com/q?x=1&y=2", "https://proxy.golang.org,direct&quot")

	assertStrings(t, "links of /a", repo.strings(t, `MATCH (:Page {url: "https://example.com/a"})-[:LINKS_TO]->(p) RETURN p.url AS v`, nil),
		"https://example.com/q?x=1&y=2")

	assertStrings(t, "aliases", repo.strings(t, `MATCH (a:Alias) RETURN a.url AS v`, nil), "http://example.com/a")

	again, err := repo.EnsureSchema(context.Background(), nil)
	if err != nil || len(again) != 0 {
		t.Errorf("EnsureSchema at the current version = %v, %v; want a no-op", again, err)
	}
}
