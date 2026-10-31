// Package uploads keeps the files users upload to their labs. An uploaded file
// is crawled and indexed like any lab source, under an address on a host that
// can never be a real site: the crawler reads it from here, and searches that
// are not restricted to a lab's own pages leave that host out.
package uploads

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"web-crawler/internal/networker"
	"web-crawler/internal/utils"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Host is reserved (RFC 2606): no page on the web can have this address.
const Host = "uploads.arachne.invalid"

const prefix = "https://" + Host + "/"

// MaxBytes matches the crawler's limit on a fetched body.
const MaxBytes = 32 << 20

var ErrNotFound = errors.New("no such upload")

var idPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func URL(id string) string {
	return prefix + id
}

func Is(url string) bool {
	return strings.HasPrefix(url, prefix)
}

// ID is the upload an address names, if it names one.
func ID(url string) (string, bool) {
	id, ok := strings.CutPrefix(url, prefix)
	return id, ok && idPattern.MatchString(id)
}

type File struct {
	ID          string
	Name        string
	ContentType string
	Data        []byte
}

// Store keeps uploads in Postgres. Purge removes a page's chunks from the
// index; it is the search side's to give, the crawler only reads.
type Store struct {
	pool  *pgxpool.Pool
	purge func(ctx context.Context, url string) error
}

func NewStore(pool *pgxpool.Pool, purge func(ctx context.Context, url string) error) *Store {
	return &Store{pool: pool, purge: purge}
}

// Save keeps the file and returns its address. The same file uploaded again
// by the same user is the same upload: indexed once, whichever labs hold it.
func (s *Store) Save(ctx context.Context, ownerID int64, name, contentType string, data []byte) (string, error) {
	id, err := utils.GenerateID()
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	err = s.pool.QueryRow(ctx, `
		INSERT INTO uploads (id, owner_id, sha256, filename, content_type, size, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owner_id, sha256) DO UPDATE SET owner_id = EXCLUDED.owner_id
		RETURNING id`, id, ownerID, sum[:], name, contentType, len(data), data).Scan(&id)
	if err != nil {
		return "", err
	}

	return URL(id), nil
}

// Owned returns the file if it is the user's.
func (s *Store) Owned(ctx context.Context, ownerID int64, id string) (File, error) {
	f := File{ID: id}

	err := s.pool.QueryRow(ctx, `SELECT filename, content_type, data FROM uploads WHERE id = $1 AND owner_id = $2`, id, ownerID).
		Scan(&f.Name, &f.ContentType, &f.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, ErrNotFound
	}

	return f, err
}

// Forget deletes, of these addresses, the owner's uploads that none of their
// labs holds any more - the file, its page, its review and its chunks.
func (s *Store) Forget(ctx context.Context, ownerID int64, urls []string) error {
	var ids []string

	for _, u := range urls {
		if id, ok := ID(u); ok {
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		return nil
	}

	var gone []string

	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			DELETE FROM uploads u WHERE u.owner_id = $1 AND u.id = ANY($2)
			  AND NOT EXISTS (SELECT 1 FROM sources s JOIN labs l ON l.id = s.lab_id
			                  WHERE l.owner_id = $1 AND s.url = $3 || u.id)
			RETURNING $3 || u.id`, ownerID, ids, prefix)
		if err != nil {
			return err
		}

		if gone, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `DELETE FROM pages WHERE url = ANY($1)`, gone); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `DELETE FROM page_reviews WHERE url = ANY($1)`, gone)

		return err
	})
	if err != nil {
		return err
	}

	// After the rows: should this fail, the chunks are left, but no search can
	// reach them - only a lab's pages may include this host, and no lab holds
	// the page now.
	for _, u := range gone {
		if err := s.purge(ctx, u); err != nil {
			return fmt.Errorf("removing %s from the index: %w", u, err)
		}
	}

	return nil
}

// Fetcher serves upload addresses from Postgres, everything else from next.
type Fetcher struct {
	next networker.Networker
	pool *pgxpool.Pool
}

func NewFetcher(next networker.Networker, pool *pgxpool.Pool) *Fetcher {
	return &Fetcher{next: next, pool: pool}
}

const readTimeout = 30 * time.Second

func (f *Fetcher) Fetch(url string) (*networker.FetchResult, error) {
	if !Is(url) {
		return f.next.Fetch(url)
	}

	// robots.txt included: there is none, so everything is allowed.
	res := &networker.FetchResult{FinalURL: url, IdentityURL: url, Status: http.StatusNotFound}

	id, ok := ID(url)
	if !ok {
		return res, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()

	err := f.pool.QueryRow(ctx, `SELECT data, content_type, filename FROM uploads WHERE id = $1`, id).Scan(&res.Body, &res.ContentType, &res.Filename)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return res, nil
	case err != nil:
		return nil, err
	}

	res.Status = http.StatusOK

	return res, nil
}

func (f *Fetcher) Stop() {
	f.next.Stop()
}
