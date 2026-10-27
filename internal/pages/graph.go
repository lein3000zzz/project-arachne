package pages

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

// A page is one :Page node per canonical URL. A URL the server redirects to that
// page permanently is an :Alias pointing at it, not a second :Page: links to the
// alias are drawn to the page itself, so link counts and PageRank are not split.
const (
	dropAliasOfPage = `MATCH (a:Alias {url: $url}) DELETE a`

	upsertPage = `
		MERGE (p:Page {url: $url})
		ON CREATE SET p.foundAt = $foundAt
		SET p.lastUpdatedAt = $lastUpdatedAt, p.status = $status, p.links = $links, p.lastRunID = $lastRunID,
			p.contentType = $contentType, p.title = $title
		WITH p
		UNWIND $links AS link
		OPTIONAL MATCH (a:Alias {url: link})
		WITH p, coalesce(a.target, link) AS target
		WHERE target <> $url
		MERGE (l:Page {url: target})
		ON CREATE SET l.foundAt = $foundAt
		MERGE (p)-[:LINKS_TO]->(l)
	`

	recordAlias = `
		MERGE (a:Alias {url: $alias})
		SET a.target = $url
		WITH a
		MATCH (chained:Alias {target: $alias})
		SET chained.target = $url
	`

	// mergeNodeBody folds node old into node keep: edges move over, and keep takes
	// old's properties if old was crawled more recently. Self-loops are dropped.
	mergeNodeBody = `
		WITH old, keep
		CALL (old, keep) {
			MATCH (src:Page)-[r:LINKS_TO]->(old)
			WHERE src <> keep AND src <> old
			MERGE (src)-[:LINKS_TO]->(keep)
			DELETE r
		}
		CALL (old, keep) {
			MATCH (old)-[r:LINKS_TO]->(dst:Page)
			WHERE dst <> keep AND dst <> old
			MERGE (keep)-[:LINKS_TO]->(dst)
			DELETE r
		}
		WITH old, keep, old.lastUpdatedAt IS NOT NULL
			AND (keep.lastUpdatedAt IS NULL OR old.lastUpdatedAt > keep.lastUpdatedAt) AS oldIsNewer
		FOREACH (_ IN CASE WHEN oldIsNewer THEN [1] ELSE [] END | SET keep += properties(old))
		SET keep.url = $url
		DETACH DELETE old
	`

	mergeAliasNode = `
		MATCH (old:Page {url: $alias})
		MERGE (keep:Page {url: $url})
	` + mergeNodeBody

	mergeNodeByID = `
		MATCH (old:Page) WHERE elementId(old) = $old
		MATCH (keep:Page) WHERE elementId(keep) = $keep
	` + mergeNodeBody
)

func savePage(ctx context.Context, tx neo4j.ManagedTransaction, params map[string]any, aliases []string) error {
	url := params["url"]

	if err := run(ctx, tx, dropAliasOfPage, map[string]any{"url": url}); err != nil {
		return err
	}

	if err := run(ctx, tx, upsertPage, params); err != nil {
		return err
	}

	for _, alias := range aliases {
		aliasParams := map[string]any{"url": url, "alias": alias}

		if err := run(ctx, tx, mergeAliasNode, aliasParams); err != nil {
			return err
		}

		if err := run(ctx, tx, recordAlias, aliasParams); err != nil {
			return err
		}
	}

	return nil
}

func run(ctx context.Context, tx neo4j.ManagedTransaction, query string, params map[string]any) error {
	result, err := tx.Run(ctx, query, params)
	if err != nil {
		return err
	}

	_, err = result.Consume(ctx)

	return err
}
