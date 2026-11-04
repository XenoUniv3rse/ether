package availability

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// publishHidden leaves older generations available to requests already using
// them. The pointer moves only after every member of the new set is stored.
func (s *Service) publishHidden(ctx context.Context, kind, policy string, keys []core.RatingKey) error {
	sorted := append([]core.RatingKey(nil), keys...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	hash := sha256.New()
	var buf [8]byte
	for _, key := range sorted {
		binary.BigEndian.PutUint64(buf[:], uint64(key))
		_, _ = hash.Write(buf[:])
	}
	gen := hex.EncodeToString(hash.Sum(nil))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = store.LockKey(ctx, store.DialectOf(s.db), tx, "availability-hidden-"+kind); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, key := range sorted {
		if _, err = tx.ExecContext(ctx, `INSERT INTO availability_hidden(kind,generation,rating_key,created_at) VALUES(?,?,?,?) ON CONFLICT(kind,generation,rating_key) DO UPDATE SET created_at=excluded.created_at`, kind, gen, int64(key), now); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO availability_hidden_current(kind,generation,policy) VALUES(?,?,?) ON CONFLICT(kind) DO UPDATE SET generation=excluded.generation,policy=excluded.policy`, kind, gen, policy); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM availability_hidden WHERE kind=? AND generation<>? AND created_at<?`, kind, gen, now-int64(time.Hour.Seconds())); err != nil {
		return err
	}
	return tx.Commit()
}

// PagingFilter captures the current immutable SQL generations once. On a new
// installation the first set is computed before it can be used for a page.
func (s *Service) PagingFilter(ctx context.Context, f core.Filter) (core.Filter, error) {
	if f.IncludeKeys != nil {
		return f, nil
	}
	input := s.snapshotFor(ctx)
	f.DigitalDiscovery = input.availability.EffectiveMode() == config.ModeDigital
	f.DigitalDateEvidence = input.availability.DigitalDateEvidence
	var kinds []string
	if f.Type != core.TypeMovie {
		kinds = append(kinds, hiddenShows)
	}
	if (f.Type == "" || f.Type == core.TypeMovie) && !f.DigitalDiscovery && !input.availability.DisableMovieEvidenceGate {
		kinds = append(kinds, hiddenMovies)
	}
	if f.Type == core.TypeEpisode {
		kinds = append(kinds, hiddenEpisodes)
	}
	for _, kind := range kinds {
		var gen, policy string
		err := s.db.QueryRowContext(ctx, `SELECT generation,policy FROM availability_hidden_current WHERE kind=?`, kind).Scan(&gen, &policy)
		want := hiddenPolicy(kind, input)
		if err == nil && policy != want {
			if _, e := s.waitHidden(ctx, kind, input, true); e != nil {
				return f, e
			}
			err = s.db.QueryRowContext(ctx, `SELECT generation,policy FROM availability_hidden_current WHERE kind=?`, kind).Scan(&gen, &policy)
			if err == nil && policy != want {
				return f, fmt.Errorf("availability: %s policy changed during refresh", kind)
			}
		}
		if err != nil {
			if err != sql.ErrNoRows {
				return f, err
			}
			// waitHidden starts (or joins) a bounded refresh and propagates failures.
			if _, e := s.waitHidden(ctx, kind, input, true); e != nil {
				return f, fmt.Errorf("availability: initialize %s: %w", kind, e)
			}
			if e := s.db.QueryRowContext(ctx, `SELECT generation FROM availability_hidden_current WHERE kind=?`, kind).Scan(&gen); e != nil {
				return f, e
			}
		} else {
			s.currentHidden(kind, input)
		}
		switch kind {
		case hiddenMovies:
			f.HiddenMovieGeneration = gen
		case hiddenShows:
			f.HiddenShowGeneration = gen
		case hiddenEpisodes:
			f.HiddenEpisodeGeneration = gen
		}
	}
	return f, nil
}

func hiddenPolicy(kind string, input discoverySnapshot) string {
	sum := sha256.Sum256([]byte(hiddenKey(kind, input)))
	return hex.EncodeToString(sum[:])
}
