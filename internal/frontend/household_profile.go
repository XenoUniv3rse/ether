package frontend

import (
	"context"
	"errors"

	"github.com/braedonsaunders/ether/internal/core"
)

// ErrHouseholdProfilePIN identifies a missing or incorrect Plex Home PIN for a
// protected profile. Frontends map it to 401.
var ErrHouseholdProfilePIN = errors.New("frontend: Plex Home PIN missing or incorrect")

// ErrHouseholdProfileIneligible identifies a profile that is not an active viewer
// eligible for the selection in progress. Frontends map it to 403.
var ErrHouseholdProfileIneligible = errors.New("frontend: Plex Home profile not eligible")

// HouseholdProfile is one selectable profile: an active Ether viewer mapped
// to a member of a Plex Home. Name is the viewer's display name; Protected
// mirrors plex.tv PIN protection for that Home member and Restricted its
// managed/restricted status. Thumb is the host-internal avatar source (a
// plex.tv thumb URL that may require authentication): frontends must proxy
// it through FetchHouseholdAvatar and never expose it to clients.
type HouseholdProfile struct {
	ViewerID   int64
	PlexID     int64
	Name       string
	Thumb      string
	Protected  bool
	Restricted bool
}

// HouseholdAvatarRequest asks the host to fetch one selected profile's
// avatar bytes. PlexToken and ViewerID select the credential source exactly
// as in HouseholdProfileSignIn; the token must never be logged or placed
// in a client-visible URL.
type HouseholdAvatarRequest struct {
	PlexToken string
	ViewerID  int64
	Profile   HouseholdProfile
}

// HouseholdProfileSignIn asks for a grant for one selected profile. Exactly
// one credential source is used: PlexToken is the initiating account token
// a client presented at sign-in (held server-side by the frontend for the
// selection's lifetime), or, when it is empty, ViewerID names the signed-in
// viewer whose retained Plex Home link the host switches within.
type HouseholdProfileSignIn struct {
	PlexToken string
	ViewerID  int64
	Profile   HouseholdProfile
	PIN       string
	Device    core.Device
}

// HouseholdProfileAvailability reads signed-in switching availability from
// cached membership. Unknown eligibility preserves the server capability.
type HouseholdProfileAvailability interface {
	PlexHomeSwitchAvailable(*Viewer) (available, known bool)
}

// HouseholdProfileAuth is optionally implemented by a surface identity that
// can list and complete Plex Home profile selections. Plex tokens and PINs
// passed to these methods must never be logged or persisted in plain text.
type HouseholdProfileAuth interface {
	// HouseholdProfiles authenticates the initiating account token and
	// returns the eligible profiles in its Plex Home.
	HouseholdProfiles(context.Context, string) ([]HouseholdProfile, error)
	// HouseholdProfilesForViewer returns the eligible profiles in the
	// signed-in viewer's Plex Home, using the host's retained account link.
	HouseholdProfilesForViewer(context.Context, *core.Viewer) ([]HouseholdProfile, error)
	// SignInWithHouseholdProfile verifies the selection (including the PIN
	// through plex.tv for protected profiles) and returns an access grant
	// for the selected viewer on the bound surface and device.
	SignInWithHouseholdProfile(context.Context, HouseholdProfileSignIn) (string, error)
	// FetchHouseholdAvatar returns the selected profile's avatar image
	// bytes and content type, fetched host-side from Profile.Thumb with
	// SSRF and size bounds. The thumb URL itself never reaches clients.
	FetchHouseholdAvatar(context.Context, HouseholdAvatarRequest) ([]byte, string, error)
}
