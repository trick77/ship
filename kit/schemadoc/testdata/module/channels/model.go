package channels

import "time"

// Channel is one row of the channels table.
// Subscribers is 0 when the count is hidden.
//
// The second paragraph is not part of the summary.
//
// Table: channels
type Channel struct {
	ID string
	// Name is the display name.
	// A second doc line is dropped.
	Name        string
	Subscribers int64 // trailing comment, used when there is no doc
	AddedAt     *time.Time
	Tags        []string
	Extra       map[string]any
	private     string
	time.Duration
}

// Stats is computed per request, never stored.
//
// Table: -
type Stats struct {
	Count int
}

type (
	// Video is one row of videos, declared in a group.
	//
	// Table: videos
	Video struct {
		ID        string
		ChannelID string `json:"channel_id"`
		A, B      int
	}

	// NotAStruct is skipped.
	NotAStruct int
)

// legacyRow is unexported and still listed, with its unexported fields; a blank field is not.
//
// Table: channels
type legacyRow struct {
	id   string
	_    [0]func()
	*row // embedded, unexported
}

// Untagged has no Table line.
type Untagged struct{}
