package pinterest

import "github.com/bytedance/sonic"

// MediaItem represents either an image or a video from search results
type MediaItem struct {
	PinID            string `json:"pin_id" msgpack:"id"`                 // Pinterest pin ID
	AggregatedPinID  string `json:"aggregated_pin_id" msgpack:"aid"`     // Aggregated pin ID for comments
	URL              string `json:"url" msgpack:"u"`                     // Original Image URL or video thumbnail URL
	FallbackURL      string `json:"fallback_url,omitempty" msgpack:"fu"` // JPEG fallback for HEIC
	IsHEIC           bool   `json:"is_heic" msgpack:"ih"`                // true if original is HEIC
	Title            string `json:"title" msgpack:"ti"`                  // Pin title
	Description      string `json:"description" msgpack:"desc"`          // Pin description
	AuthorName       string `json:"author_name" msgpack:"an"`            // Creator name
	AuthorUsername   string `json:"author_username" msgpack:"au"`        // Creator username
	AuthorAvatar     string `json:"author_avatar" msgpack:"a"`           // Creator avatar URL
	AuthorAvatarFallback string `json:"author_avatar_fallback" msgpack:"af"` // Creator avatar fallback URL
	Saves            int    `json:"saves" msgpack:"s"`                   // Number of saves
	Likes            int    `json:"likes" msgpack:"l"`                   // Number of likes (reactions)
	Comments         int    `json:"comments" msgpack:"co"`               // Number of comments
	Repins           int    `json:"repins" msgpack:"r"`                  // Number of repins
	CommentsDisabled bool   `json:"comments_disabled" msgpack:"cd"`      // true if comments are disabled
	IsVideo          bool   `json:"is_video" msgpack:"v"`                // true if this is a video pin
	VideoURL         string `json:"video_url,omitempty" msgpack:"vu"`    // HLS or MP4 video URL
	ThumbnailURL     string `json:"thumbnail_url,omitempty" msgpack:"t"` // Video thumbnail
	Duration         int    `json:"duration,omitempty" msgpack:"d"`      // Video duration in milliseconds
	Width            int    `json:"width,omitempty" msgpack:"w"`
	Height           int    `json:"height,omitempty" msgpack:"h"`
}

// UserProfile represents a Pinterest user profile
type UserProfile struct {
	Username       string `json:"username"`
	FullName       string `json:"full_name"`
	AvatarURL      string `json:"avatar_url"`
	AvatarFallback string `json:"avatar_fallback,omitempty"`
	FollowerCount  int    `json:"follower_count"`
	FollowingCount int    `json:"following_count"`
	BoardCount     int    `json:"board_count"`
	About          string `json:"about"`
}

// Board represents a Pinterest board
type Board struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	Slug        string `json:"slug"`
	PinCount          int    `json:"pin_count"`
	Thumbnail         string `json:"thumbnail"`
	ThumbnailFallback string `json:"thumbnail_fallback,omitempty"`
	Description       string `json:"description"`
}

type SearchResult struct {
	Media     []MediaItem `json:"media,omitempty" msgpack:"m"`   // All media (images + videos)
	Boards    []Board     `json:"boards,omitempty" msgpack:"bs"` // Board search results
	Users     []UserProfile `json:"users,omitempty" msgpack:"us"` // User search results
	Board     *Board      `json:"board,omitempty" msgpack:"bd"`  // Board metadata (if in board context)
	Bookmark  string      `json:"bookmark,omitempty" msgpack:"b"`
	CSRFToken string      `json:"csrf_token,omitempty" msgpack:"c"`
}

type Comment struct {
	ID             string         `json:"id" msgpack:"id"`
	Type           string         `json:"type" msgpack:"ty"`           // "aggregatedcomment" or "userdiditdata"
	Text           string         `json:"text" msgpack:"t"`           // main text for aggregatedcomment
	Details        string         `json:"details" msgpack:"d"`        // main text for userdiditdata
	AuthorName     string         `json:"author_name" msgpack:"an"`
	AuthorUsername string         `json:"author_username" msgpack:"aun"`
	AuthorAvatar   string         `json:"author_avatar" msgpack:"av"`
	CreatedAt      string         `json:"created_at" msgpack:"ca"`
	Likes          int            `json:"likes" msgpack:"l"`
	ReplyCount     int            `json:"reply_count" msgpack:"rc"`
	Images         []CommentImage `json:"images" msgpack:"ims"`
}

type CommentImage struct {
	URL    string `json:"url" msgpack:"u"`
	Width  int    `json:"width" msgpack:"w"`
	Height int    `json:"height" msgpack:"h"`
}

type CommentResult struct {
	Comments        []Comment `json:"comments" msgpack:"cs"`
	Bookmark        string    `json:"bookmark" msgpack:"b"`
	AggregatedPinID string    `json:"aggregated_pin_id" msgpack:"aid"`
}

// Internal Pinterest response types
type pinterestResponse struct {
	ResourceResponse struct {
		Data     pinterestResult `json:"data"`
		Bookmark string          `json:"bookmark"`
	} `json:"resource_response"`
}

type pinterestResult struct {
	ID            string            `json:"id"`
	EntityId      string            `json:"entity_id"`
	Name          string            `json:"name"`
	URL           string            `json:"url"`
	PinCount      int               `json:"pin_count"`
	Title         FlexString        `json:"title"`
	BoardTitle    string            `json:"board_title"`
	Label         string            `json:"label"`
	GridTitle     string            `json:"grid_title"`
	Description   string            `json:"description"`
	CommentCount  int               `json:"comment_count"`
	RepinCount    int               `json:"repin_count"`
	RepinCountAlt int               `json:"repinCount"`
	Results       []pinterestResult `json:"results"`
	Username      string            `json:"username"`
	FullName      string            `json:"full_name"`
	ImageMediumURL string           `json:"image_medium_url"`
	FollowerCount int               `json:"follower_count"`
	About         string            `json:"about"`
	Images        map[string]sonic.NoCopyRawMessage `json:"images"`
	Videos       *videoData    `json:"videos"`
	StoryPinData *storyPinData `json:"story_pin_data"`
	Pinner       *struct {
		Username       string `json:"username"`
		FullName       string `json:"full_name"`
		ImageMediumURL string `json:"image_medium_url"`
		ImageSmallURL  string `json:"image_small_url"`
	} `json:"pinner"`
	CloseupAttribution *struct {
		FullName       string `json:"full_name"`
		ImageMediumURL string `json:"image_medium_url"`
	} `json:"closeup_attribution"`
	ReactionCounts map[string]int `json:"reaction_counts"`
	ReactionCountsData []struct {
		ReactionType  int `json:"reactionType"`
		ReactionCount int `json:"reactionCount"`
	} `json:"reactionCountsData"`

	CommentsDisabled    bool `json:"comments_disabled"`
	CommentsDisabledAlt bool `json:"commentsDisabled"`

	AggregatedPinData    *aggregatedData    `json:"aggregated_pin_data"`
	AggregatedPinDataAlt *aggregatedData    `json:"aggregatedPinData"`
	ImageCoverURL        string             `json:"image_cover_url"`
	IsPromoted           bool               `json:"is_promoted"`
	Method               string             `json:"method"`
	RichSummary          *struct {
		TypeName string `json:"type_name"`
		Products []interface{} `json:"products"`
	} `json:"rich_summary"`
}

type aggregatedData struct {
	ID       string     `json:"id"`
	Stats    *statsData `json:"aggregated_stats"`
	StatsAlt *statsData `json:"aggregatedStats"`
}

type statsData struct {
	Saves        int `json:"saves"`
	Comments     int `json:"comments"`
	CommentCount int `json:"comment_count"`
}

type storyPinData struct {
	TotalVideoDuration int         `json:"total_video_duration"`
	Pages              []storyPage `json:"pages"`
}

type storyPage struct {
	Blocks []storyBlock `json:"blocks"`
}

type storyBlock struct {
	BlockType int        `json:"block_type"`
	Video     *videoData `json:"video"`
}

type videoData struct {
	ID        string     `json:"id"`
	VideoList *videoList `json:"video_list"`
}

type videoList struct {
	Variants map[string]videoVariant `json:"-"`
}

type videoVariant struct {
	URL       string `json:"url"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Duration  int    `json:"duration"`
	Thumbnail string `json:"thumbnail"`
}

func (vl *videoList) UnmarshalJSON(data []byte) error {
	vl.Variants = make(map[string]videoVariant)
	return sonic.Unmarshal(data, &vl.Variants)
}

// FlexString handles string or object titles
type FlexString string

func (fs *FlexString) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := sonic.Unmarshal(data, &s); err != nil {
			return err
		}
		*fs = FlexString(s)
		return nil
	}
	var m struct {
		Text   string `json:"text"`
		Format string `json:"format"`
	}
	if err := sonic.Unmarshal(data, &m); err == nil {
		if m.Text != "" {
			*fs = FlexString(m.Text)
		} else {
			*fs = FlexString(m.Format)
		}
		return nil
	}
	return nil // Silent ignore for incompatible objects
}
