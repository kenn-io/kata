package forgejo

import "time"

// These types model Forgejo's JSON for the fake providers. They are
// independent of the SDK types so fixtures exercise the real wire contract.
type user struct {
	ID       int64  `json:"id"`
	Login    string `json:"login"`
	FullName string `json:"full_name"`
}
type repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}
type label struct {
	ID int64 `json:"id"`
}
type issue struct {
	ID          int64       `json:"id"`
	Number      int64       `json:"number"`
	Title       string      `json:"title"`
	Body        string      `json:"body"`
	State       string      `json:"state"`
	UpdatedAt   time.Time   `json:"updated_at"`
	User        *user       `json:"user"`
	Repository  *repository `json:"repository"`
	PullRequest any         `json:"pull_request"`
	DueDate     *time.Time  `json:"due_date"`
	Labels      []label     `json:"labels"`
	Assignees   []user      `json:"assignees"`
	Milestone   *label      `json:"milestone"`
}
type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      *user     `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// appliedOptions keeps only the request options Forgejo applies: it skips null
// options and ignores an empty issue title.
func appliedOptions(raw map[string]any) map[string]any {
	edited := map[string]any{}
	for key, value := range raw {
		if value == nil || (key == "title" && value == "") {
			continue
		}
		edited[key] = value
	}
	return edited
}
