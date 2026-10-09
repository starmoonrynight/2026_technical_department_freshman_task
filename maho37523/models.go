package main

type UpdateItemStatusRequest struct {
	Status string `json:"status"`
}

type Response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type CreateItemRequest struct {
	ItemType    string   `json:"type"`
	Name        string   `json:"name"`
	Location    string   `json:"location"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Color       string   `json:"color"`
	Brand       string   `json:"brand"`
	Tags        []string `json:"tags"`
	Contact     string   `json:"contact"`
	OccurredAt  int64    `json:"occurred_at"`
	ImageIDs    []int    `json:"image_ids"`
	AllowAI     bool     `json:"allow_ai"`
}

type Item struct {
	ID          int      `json:"id"`
	ItemType    string   `json:"type"`
	Name        string   `json:"name"`
	Location    string   `json:"location"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	UserID      int      `json:"user_id"`
	Category    string   `json:"category"`
	Color       string   `json:"color"`
	Brand       string   `json:"brand"`
	Tags        []string `json:"tags"`
	Contact     string   `json:"contact,omitempty"`
	OccurredAt  int64    `json:"occurred_at"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
	Revision    int      `json:"revision"`
	AllowAI     bool     `json:"allow_ai"`
	Images      []Media  `json:"images"`
}

type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type ItemListResponse struct {
	Items    []Item `json:"items"`
	Total    int    `json:"total"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
}

type SearchQuery struct {
	Keyword, Breadth, ItemType, Status string
	Page, PageSize                     int
	ForMatching                        bool
	ExcludeUserID                      int
}
type Media struct {
	ID        int    `json:"id"`
	UserID    int    `json:"-"`
	Path      string `json:"-"`
	URL       string `json:"url"`
	MIME      string `json:"mime"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	CreatedAt int64  `json:"created_at"`
}
type ItemSuggestion struct {
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Color       string   `json:"color"`
	Brand       string   `json:"brand"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}
type MatchJudgement struct {
	Possible    bool     `json:"possible"`
	Score       float64  `json:"score"`
	Reasons     []string `json:"reasons"`
	Conflicts   []string `json:"conflicts"`
	Uncertainty string   `json:"uncertainty"`
}
type AIJob struct {
	ID       int    `json:"id"`
	Kind     string `json:"kind"`
	UserID   int    `json:"-"`
	ItemID   int    `json:"item_id,omitempty"`
	MediaID  int    `json:"media_id,omitempty"`
	Revision int    `json:"revision,omitempty"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Result   string `json:"-"`
	Error    string `json:"error,omitempty"`
}
type Notification struct {
	ID          int      `json:"id"`
	LostID      int      `json:"lost_id"`
	FoundID     int      `json:"found_id"`
	FoundName   string   `json:"found_name"`
	Score       float64  `json:"score"`
	Reasons     []string `json:"reasons"`
	Conflicts   []string `json:"conflicts"`
	Uncertainty string   `json:"uncertainty"`
	Available   bool     `json:"available"`
	ReadAt      int64    `json:"read_at"`
	CreatedAt   int64    `json:"created_at"`
}
