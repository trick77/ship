package store

// Video is a row of videos.
//
// Table: videos
type Video struct {
	ID    int64
	Title string
}

type (
	// Count is an aggregate.
	//
	// Table: -
	Count struct{ N int }
	// Channel is a row of channels.
	//
	// Table: channels
	Channel struct{ ID int64 }
	// Tag is a row of tags: Tags is no table name.
	//
	// Table: Tags
	Tag struct{ ID int64 }
)

// Note is a row of notes, its table unnamed.
type Note struct{ ID int64 }
