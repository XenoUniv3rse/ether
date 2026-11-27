package perfharness

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func addMatrixPlexLiveCoverage(c *Client) {
	const provider = "/" + frontend.LiveTVProvider
	for i := range c.Steps {
		step := &c.Steps[i]
		switch step.Name {
		case "live guide grid 3h":
			step.Discover = func(vars map[string]string, body []byte) {
				var key string
				if json.Unmarshal(plexMetadata0(body)["ratingKey"], &key) == nil {
					setOnce(vars, "plexProgramme", url.PathEscape(key))
				}
			}
		case "/livetv/dvrs":
			step.Discover = discoverMatrixPlexDVR
		}
	}
	c.Steps = append(c.Steps, Step{Name: "live channels", Class: classGuide, Path: "/livetv/epg/channels", Discover: discoverMatrixPlexChannel})
	for _, path := range []string{
		"/livetv/dvrs/{plexDVR}", "/media/grabbers", "/media/grabbers/devices",
		"/media/grabbers/devices/{plexTuner}", "/media/grabbers/devices/{plexTuner}/channelmap",
		"/media/grabbers/devices/{plexTuner}/channels", "/media/subscriptions", "/livetv/sessions",
		"/livetv/epg/channels", "/livetv/epg/channelmap", "/livetv/epg/lineupchannels",
		"/livetv/guide.xml?from={guideFrom}&until={guideUntil}",
		"/livetv/channels/{plexChannel}/logo.png", "/livetv/channels/{plexChannel}/logo.svg",
		"/{plexSignedLogo}",
		provider, provider + "/sections", provider + "/sections/{plexDVR}/all",
		provider + "/grid?type=1&beginsAt%3C={now3h}&endsAt%3E={now}", provider + "/lineups/dvr/channels",
		provider + "/lineups/dvr/channels/featured", provider + "/categories/sports/channels",
		provider + "/channels/{plexChannel}", provider + "/metadata/{plexProgramme}",
		provider + "/hubs?count=12", provider + "/hubs/sections/home?count=12",
		provider + "/hubs/discover?count=12", provider + "/hubs/promoted?count=12",
		provider + "/library/search?query=Fixture&limit=24",
		provider + "/watchnow", provider + "/watchnow/all?limit=24",
		provider + "/watchnow/tonight?limit=24", provider + "/watchnow/categories/sports?limit=24",
	} {
		if path != "/livetv/epg/channels" && path != provider+"/hubs?count=12" && !strings.HasPrefix(path, provider+"/grid?") {
			c.Steps = append(c.Steps, Step{Name: "live GET " + path, Class: classGuide, Path: path})
		}
		c.Steps = append(c.Steps, Step{Name: "live HEAD " + path, Class: classGuide, Method: http.MethodHead, Path: path})
	}
}

func discoverMatrixPlexDVR(vars map[string]string, body []byte) {
	var result struct {
		MediaContainer struct {
			DVR []struct {
				Key    string `json:"key"`
				Device []struct {
					Key string `json:"key"`
				} `json:"Device"`
			} `json:"Dvr"`
		} `json:"MediaContainer"`
	}
	if json.Unmarshal(body, &result) == nil && len(result.MediaContainer.DVR) > 0 {
		dvr := result.MediaContainer.DVR[0]
		setOnce(vars, "plexDVR", url.PathEscape(dvr.Key))
		if len(dvr.Device) != 0 {
			setOnce(vars, "plexTuner", url.PathEscape(dvr.Device[0].Key))
		}
	}
}

func discoverMatrixPlexChannel(vars map[string]string, body []byte) {
	var result struct {
		MediaContainer struct {
			Channels []struct {
				ID    string `json:"id"`
				Thumb string `json:"thumb"`
			} `json:"Channel"`
		} `json:"MediaContainer"`
	}
	if json.Unmarshal(body, &result) == nil && len(result.MediaContainer.Channels) != 0 {
		channel := result.MediaContainer.Channels[0]
		setOnce(vars, "plexChannel", url.PathEscape(channel.ID))
		if thumb, err := url.Parse(channel.Thumb); err == nil && strings.HasPrefix(thumb.Path, "/livetv/logos/") {
			setOnce(vars, "plexSignedLogo", strings.TrimPrefix(thumb.EscapedPath(), "/"))
		}
	}
}
