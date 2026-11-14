package model

import "time"

// BackfillPlan 是补数前的预估：一共几天、已有几天、真要补几天。
type BackfillPlan struct {
	Pipeline string   `json:"pipeline"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Total    int      `json:"total"`
	Existing int      `json:"existing"`
	Missing  int      `json:"missing"`
	Dates    []string `json:"dates"`
	Skipped  []string `json:"skipped"`
}

// BackfillBatch 是一次补数提交。进度不落库、按实例反查，
// 这样「实例状态」永远是唯一事实源，不会出现批次表和实例表打架。
type BackfillBatch struct {
	ID         string    `json:"id"`
	Pipeline   string    `json:"pipeline"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Total      int       `json:"total"`
	Created    int       `json:"created"`
	Skipped    int       `json:"skipped"`
	Reason     string    `json:"reason"`
	CreatedBy  string    `json:"createdBy"`
	CreatedAt  time.Time `json:"createdAt"`
	Success    int       `json:"success"`
	Failed     int       `json:"failed"`
	Running    int       `json:"running"`
	Done       int       `json:"done"`
	Percent    float64   `json:"percent"`
	FailDetail []string  `json:"failDetail,omitempty"`
}
