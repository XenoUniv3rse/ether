package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The lab client has a random client identifier, so its queue cannot include
// a viewer's existing items. Remove only items created by this invocation.
func (l *labClient) download(ctx context.Context, key string, check func([]byte, string) error) error {
	data, _, err := l.request(ctx, http.MethodPost, "/downloadQueue")
	if err != nil {
		return fmt.Errorf("create download queue: %w", err)
	}
	var queue struct {
		MediaContainer struct {
			Queues []struct {
				ID int64 `json:"id"`
			} `json:"DownloadQueue"`
		} `json:"MediaContainer"`
	}
	if json.Unmarshal(data, &queue) != nil || len(queue.MediaContainer.Queues) != 1 || queue.MediaContainer.Queues[0].ID <= 0 {
		return fmt.Errorf("invalid download queue")
	}
	base := "/downloadQueue/" + strconv.FormatInt(queue.MediaContainer.Queues[0].ID, 10)
	var ids []int64
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Also discover an item whose enqueue succeeded but first poll failed.
		data, _, err := l.get(cleanup, base+"/items")
		if err == nil {
			var current struct {
				MediaContainer struct {
					Items []struct {
						ID int64 `json:"id"`
					} `json:"DownloadQueueItem"`
				} `json:"MediaContainer"`
			}
			if json.Unmarshal(data, &current) == nil {
				ids = nil
				for _, item := range current.MediaContainer.Items {
					if item.ID > 0 {
						ids = append(ids, item.ID)
					}
				}
			}
		}
		for _, id := range ids {
			_, _, _ = l.request(cleanup, http.MethodDelete, fmt.Sprintf("%s/items/%d", base, id))
		}
	}()
	_, _, err = l.request(ctx, http.MethodPost, base+"/add?keys="+url.QueryEscape("/library/metadata/"+key))
	if err != nil {
		return fmt.Errorf("enqueue download: %w", err)
	}
	wait, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		data, _, err = l.get(wait, base+"/items")
		if err != nil {
			return fmt.Errorf("poll download: %w", err)
		}
		var items struct {
			MediaContainer struct {
				Items []struct {
					ID     int64  `json:"id"`
					Status string `json:"status"`
				} `json:"DownloadQueueItem"`
			} `json:"MediaContainer"`
		}
		if json.Unmarshal(data, &items) != nil {
			return fmt.Errorf("invalid download status")
		}
		ids = nil
		for _, item := range items.MediaContainer.Items {
			if item.ID > 0 {
				ids = append(ids, item.ID)
			}
		}
		if len(items.MediaContainer.Items) == 1 {
			item := items.MediaContainer.Items[0]
			switch item.Status {
			case "available":
				media, _, err := l.get(wait, fmt.Sprintf("%s/item/%d/media", base, item.ID))
				if err != nil {
					return fmt.Errorf("download media: %w", err)
				}
				return check(media, "mp4")
			case "error":
				return fmt.Errorf("download conversion failed")
			}
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("download exceeded 60 seconds")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
