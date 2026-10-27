package pages

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"web-crawler/internal/urlnorm"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// Resolver asks the server which canonical URL a page really lives at. ok is false
// when the answer is "this one" or unknown.
type Resolver func(url string) (canonical string, ok bool)

var schema = []string{
	`CREATE CONSTRAINT page_url IF NOT EXISTS FOR (p:Page) REQUIRE p.url IS UNIQUE`,
	`CREATE CONSTRAINT alias_url IF NOT EXISTS FOR (a:Alias) REQUIRE a.url IS UNIQUE`,
	`CREATE INDEX alias_target IF NOT EXISTS FOR (a:Alias) ON (a.target)`,
}

// EnsureSchema creates the constraints and keeps stored URLs canonical. A graph
// that predates the constraints, or was written under another urlnorm.Version, is
// cleaned up first. It returns the URLs that stopped being pages, so what was
// indexed under them can be dropped.
func (repo *PageNeo4jRepo) EnsureSchema(ctx context.Context, resolve Resolver) ([]string, error) {
	constrained, err := repo.constraintExists(ctx, "page_url")
	if err != nil {
		return nil, err
	}

	version, err := repo.normalizerVersion(ctx)
	if err != nil {
		return nil, err
	}

	var removed []string

	if !constrained || version != urlnorm.Version {
		repo.logger.Infow("Re-canonicalizing stored URLs", "constrained", constrained, "from", version, "to", urlnorm.Version)

		if removed, err = repo.mergeSpellings(ctx); err != nil {
			return nil, fmt.Errorf("merging URL spellings: %w", err)
		}

		if err := repo.dropStaleAliases(ctx); err != nil {
			return nil, fmt.Errorf("dropping stale aliases: %w", err)
		}
	}

	// The server is asked only once: twins it could not merge then - dead hosts,
	// sites that serve both forms - would be fetched again on every start.
	if !constrained {
		aliases, err := repo.mergeRedirects(ctx, resolve)
		if err != nil {
			return nil, fmt.Errorf("merging redirect aliases: %w", err)
		}

		removed = append(removed, aliases...)
	}

	for _, statement := range schema {
		if err := repo.exec(ctx, statement, nil); err != nil {
			return nil, fmt.Errorf("%s: %w", statement, err)
		}
	}

	if err := repo.exec(ctx, `MERGE (s:Schema {name: "urlnorm"}) SET s.version = $version`,
		map[string]any{"version": urlnorm.Version}); err != nil {
		return nil, err
	}

	return removed, nil
}

func (repo *PageNeo4jRepo) exec(ctx context.Context, query string, params map[string]any) error {
	_, err := neo4j.ExecuteQuery(ctx, repo.driver, query, params, neo4j.EagerResultTransformer,
		neo4j.ExecuteQueryWithDatabase(repo.database))

	return err
}

func (repo *PageNeo4jRepo) normalizerVersion(ctx context.Context) (int64, error) {
	res, err := neo4j.ExecuteQuery(ctx, repo.driver, `MATCH (s:Schema {name: "urlnorm"}) RETURN s.version AS version`,
		nil, neo4j.EagerResultTransformer, neo4j.ExecuteQueryWithDatabase(repo.database))
	if err != nil || len(res.Records) == 0 {
		return 0, err
	}

	version, _ := res.Records[0].Get("version")
	v, _ := version.(int64)

	return v, nil
}

// dropStaleAliases forgets aliases whose spelling the normalizer no longer
// produces. They are learned again the next time the alias is fetched.
func (repo *PageNeo4jRepo) dropStaleAliases(ctx context.Context) error {
	res, err := neo4j.ExecuteQuery(ctx, repo.driver, `MATCH (a:Alias) RETURN a.url AS url, a.target AS target`,
		nil, neo4j.EagerResultTransformer, neo4j.ExecuteQueryWithDatabase(repo.database))
	if err != nil {
		return err
	}

	var stale []string

	for _, record := range res.Records {
		aliasURL, _ := record.Get("url")
		target, _ := record.Get("target")

		for _, v := range []any{aliasURL, target} {
			s, _ := v.(string)
			if canonical, ok := urlnorm.Canonical(s, nil); !ok || canonical != s {
				stale = append(stale, aliasURL.(string))
				break
			}
		}
	}

	if len(stale) == 0 {
		return nil
	}

	return repo.exec(ctx, `MATCH (a:Alias) WHERE a.url IN $urls DELETE a`, map[string]any{"urls": stale})
}

func (repo *PageNeo4jRepo) constraintExists(ctx context.Context, name string) (bool, error) {
	res, err := neo4j.ExecuteQuery(ctx, repo.driver, `SHOW CONSTRAINTS YIELD name WHERE name = $name RETURN name`,
		map[string]any{"name": name}, neo4j.EagerResultTransformer, neo4j.ExecuteQueryWithDatabase(repo.database))
	if err != nil {
		return false, err
	}

	return len(res.Records) > 0, nil
}

type pageNode struct {
	id  string
	url string
}

func (repo *PageNeo4jRepo) pageNodes(ctx context.Context) ([]pageNode, error) {
	res, err := neo4j.ExecuteQuery(ctx, repo.driver, `MATCH (p:Page) RETURN elementId(p) AS id, p.url AS url`,
		nil, neo4j.EagerResultTransformer, neo4j.ExecuteQueryWithDatabase(repo.database))
	if err != nil {
		return nil, err
	}

	nodes := make([]pageNode, 0, len(res.Records))

	for _, record := range res.Records {
		id, _ := record.Get("id")
		u, _ := record.Get("url")

		idStr, _ := id.(string)
		urlStr, _ := u.(string)

		nodes = append(nodes, pageNode{id: idStr, url: urlStr})
	}

	return nodes, nil
}

// mergeSpellings folds every node into the one for its canonical URL: exact
// duplicates left by concurrent MERGEs, and spellings the old normalizer let through.
func (repo *PageNeo4jRepo) mergeSpellings(ctx context.Context) ([]string, error) {
	nodes, err := repo.pageNodes(ctx)
	if err != nil {
		return nil, err
	}

	groups := make(map[string][]pageNode)

	var unusable []string

	for _, node := range nodes {
		canonical, ok := urlnorm.Canonical(node.url, nil)
		if !ok {
			unusable = append(unusable, node.id)
			continue
		}

		groups[canonical] = append(groups[canonical], node)
	}

	// Link targets that can never be fetched: markup and prose the old parsers took
	// for URLs. A crawled page is kept whatever its URL looks like.
	if len(unusable) > 0 {
		if err := repo.exec(ctx, `MATCH (p:Page) WHERE elementId(p) IN $ids AND p.status IS NULL DETACH DELETE p`,
			map[string]any{"ids": unusable}); err != nil {
			return nil, err
		}

		repo.logger.Infow("Dropped placeholder pages with unusable URLs", "candidates", len(unusable))
	}

	var removed []string

	for canonical, members := range groups {
		if len(members) == 1 && members[0].url == canonical {
			continue
		}

		// Prefer the node already spelled canonically, so its ID survives.
		sort.SliceStable(members, func(i, j int) bool {
			return members[i].url == canonical && members[j].url != canonical
		})

		keep := members[0]

		_, err := repo.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
			for _, old := range members[1:] {
				if err := run(ctx, tx, mergeNodeByID, map[string]any{"old": old.id, "keep": keep.id, "url": canonical}); err != nil {
					return nil, err
				}
			}

			return nil, run(ctx, tx, `MATCH (p:Page) WHERE elementId(p) = $keep SET p.url = $url`,
				map[string]any{"keep": keep.id, "url": canonical})
		})
		if err != nil {
			return nil, err
		}

		for _, member := range members {
			if member.url != canonical {
				removed = append(removed, member.url)
			}
		}

		repo.logger.Infow("Merged URL spellings", "url", canonical, "nodes", len(members))
	}

	return removed, nil
}

// mergeRedirects asks the server about pages that differ only by scheme or a
// trailing slash - the pairs a site almost always redirects between, and the
// only ones the crawler used to keep apart. Whatever redirects permanently becomes
// an alias of where it lands.
func (repo *PageNeo4jRepo) mergeRedirects(ctx context.Context, resolve Resolver) ([]string, error) {
	nodes, err := repo.pageNodes(ctx)
	if err != nil {
		return nil, err
	}

	groups := make(map[string][]string)

	for _, node := range nodes {
		if key, ok := siblingKey(node.url); ok {
			groups[key] = append(groups[key], node.url)
		}
	}

	var aliases []string

	for _, members := range groups {
		if len(members) < 2 {
			continue
		}

		for _, member := range members {
			target, ok := resolve(member)
			if !ok || target == member {
				continue
			}

			_, err := repo.write(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
				params := map[string]any{"url": target, "alias": member}

				if err := run(ctx, tx, mergeAliasNode, params); err != nil {
					return nil, err
				}

				return nil, run(ctx, tx, recordAlias, params)
			})
			if err != nil {
				return nil, err
			}

			aliases = append(aliases, member)

			repo.logger.Infow("Merged redirect alias", "alias", member, "url", target)
		}
	}

	return aliases, nil
}

func siblingKey(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}

	return u.Host + strings.TrimSuffix(u.EscapedPath(), "/") + "?" + u.RawQuery, true
}
