package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const ResolverTotal = 25 * time.Second

type resolveSliceHookKey struct{}

// WithResolveSliceHook attaches a callback invoked immediately before each
// resolve/match slice inside Manager.Call so the caller can track per-slice
// host source progress.
func WithResolveSliceHook(ctx context.Context, fn func()) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, resolveSliceHookKey{}, fn)
}

// PendingReply defers a kind resolve or match without recording a failure.
// RetryInMS is clamped to 250..5000. Job is an optional /ether/job/ route;
// the host passes the original request body and drives job continuations.
type PendingReply struct {
	Pending   bool   `json:"pending"`
	RetryInMS int64  `json:"retryInMs,omitempty"`
	Job       string `json:"job,omitempty"`
}

func pendingBudgetBody(retryInMS int64) json.RawMessage {
	if retryInMS > 0 {
		if b, err := json.Marshal(PendingReply{Pending: true, RetryInMS: retryInMS}); err == nil {
			return b
		}
	}
	return json.RawMessage(`{"pending":true}`)
}

// PendingFallback returns the deferred preparation for a mixed resolve reply.
func (r Response) PendingFallback() func() error {
	return r.pendingFallback
}

// Call applies the total pending budget to kind resolvers and matchers.
func (m *Manager) Call(ctx context.Context, pluginID string, req Request) (Response, error) {
	if !strings.HasPrefix(req.Path, "/ether/v1/kinds/") || strings.HasSuffix(req.Path, "/live/resolve") || (!strings.HasSuffix(req.Path, "/resolve") && !strings.HasSuffix(req.Path, "/match")) {
		return m.callOnce(ctx, pluginID, req)
	}
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, ResolverTotal)
	defer cancel()
	// One job per plugin/kind/item (and part index / source when set), even across
	// simultaneous Plays. Include the request body when older callers do not
	// supply an item reference.
	var identity struct {
		PartIndex *int   `json:"partIndex"`
		Source    string `json:"source"`
		Item      struct {
			Ref        string `json:"ref"`
			ExternalID string `json:"id"`
		} `json:"item"`
	}
	_ = json.Unmarshal(req.Body, &identity)
	item := identity.Item.Ref
	if item == "" {
		item = identity.Item.ExternalID
	}
	if item == "" {
		item = string(req.Body)
	} else {
		if identity.PartIndex != nil {
			item = fmt.Sprintf("%s#part:%d", item, *identity.PartIndex)
		}
		if identity.Source != "" {
			item = fmt.Sprintf("%s#source:%s", item, identity.Source)
		}
	}
	suffix := fmt.Sprintf("|%x", sha256.Sum256([]byte(strings.TrimSuffix(strings.TrimSuffix(req.Path, "/resolve"), "/match")+"|"+item)))
	started := false
	var lastRetryMS int64
	var job *actionJob
	for {
		if onSlice, _ := ctx.Value(resolveSliceHookKey{}).(func()); onSlice != nil {
			onSlice()
		}
		resp, err := m.callOnce(ctx, pluginID, req)
		if err != nil {
			if parent.Err() != nil {
				return Response{}, parent.Err()
			}
			if ctx.Err() != nil {
				return Response{Status: 202, Body: pendingBudgetBody(lastRetryMS)}, nil
			}
			return resp, err
		}
		var pending PendingReply
		if resp.Status >= 400 || json.Unmarshal(resp.Body, &pending) != nil || !pending.Pending {
			return resp, nil
		}
		if pending.RetryInMS > 0 {
			lastRetryMS = pending.RetryInMS
		}
		if strings.HasSuffix(req.Path, "/resolve") {
			var resolved struct {
				Sources []json.RawMessage `json:"sources"`
			}
			if json.Unmarshal(resp.Body, &resolved) == nil && len(resolved.Sources) > 0 {
				body := resp.Body
				resp.pendingFallback = sync.OnceValue(func() error {
					if !started {
						if _, err := m.startPendingResolverJob(pluginID, pending.Job, body, req.Body, suffix, lastRetryMS); err != nil {
							return err
						}
					}
					return core.PreparingFromPending(lastRetryMS)
				})
				return resp, nil
			}
		}
		if !started && strings.HasPrefix(pending.Job, "/ether/job/") {
			job, err = m.startPendingResolverJob(pluginID, pending.Job, resp.Body, req.Body, suffix, lastRetryMS)
			if err != nil {
				sec := core.JobsBusyRetrySeconds(err, 5)
				return Response{
					Status: http.StatusServiceUnavailable,
					Headers: map[string]string{
						"Retry-After":         strconv.Itoa(sec),
						"X-Ether-Media-Error": "jobs_busy",
					},
					Body: core.JobsBusyErrorBody(err),
				}, err
			}
			started = true
		}
		var done <-chan struct{}
		if job != nil {
			done = job.done
		}
		timer := time.NewTimer(time.Duration(max(int64(250), min(int64(5000), pending.RetryInMS))) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			if parent.Err() != nil {
				return Response{}, parent.Err()
			}
			return Response{Status: 202, Body: pendingBudgetBody(lastRetryMS)}, nil
		case <-done:
			timer.Stop()
			job = nil
		case <-timer.C:
		}
	}
}

func (m *Manager) startPendingResolverJob(pluginID, path string, body, initial json.RawMessage, suffix string, retryMS int64) (*actionJob, error) {
	if !strings.HasPrefix(path, "/ether/job/") {
		return nil, nil
	}
	job, err := m.startResolverJob(pluginID, body, initial, suffix)
	if err != nil {
		return nil, core.JobsBusyFromPending(err.Error(), retryMS)
	}
	return job, nil
}
